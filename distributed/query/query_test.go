package query

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"pebbledb/catalog"
	"pebbledb/codec"
	"pebbledb/distributed/ranges"
	"pebbledb/sql/plan"
	"pebbledb/storage/mvcc"
	"pebbledb/storage/rowstore"
	"pebbledb/types"
)

func TestDeriveScanSpansAcrossRangeBoundaries(t *testing.T) {
	table := catalog.TableDescriptor{ID: 42, Name: "users"}
	prefix := rowstore.TablePrefix(table.ID)
	physicalSplit, _ := mvcc.PhysicalSpan(append(append([]byte(nil), prefix...), 0x80), nil)
	descriptors := []ranges.Descriptor{
		{ID: 1, End: physicalSplit, ReplicaID: 10, Generation: 3},
		{ID: 2, Start: physicalSplit, ReplicaID: 20, Generation: 1},
	}
	spans, err := DeriveScanSpans(plan.PhysicalPlan{Root: &plan.PhysicalNode{
		Kind: plan.ScanKind, Table: table, Access: plan.FullTableScan,
	}}, descriptors)
	if err != nil {
		t.Fatal(err)
	}
	if len(spans) != 2 || spans[0].RangeID != 1 || spans[1].RangeID != 2 || string(spans[0].End) != string(spans[1].Start) {
		t.Fatalf("derived spans = %+v", spans)
	}
}

func TestCoordinatorRetriesStreamsAndEnforcesAdmission(t *testing.T) {
	var active, maximum, attempts atomic.Int32
	endpoint := EndpointFunc(func(ctx context.Context, task Task) ([]Row, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			observed := maximum.Load()
			if current <= observed || maximum.CompareAndSwap(observed, current) {
				break
			}
		}
		if task.Span.RangeID == 1 && attempts.Add(1) == 1 {
			return nil, fmt.Errorf("temporary: %w", ErrRetryable)
		}
		select {
		case <-time.After(5 * time.Millisecond):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return []Row{{Values: []types.Value{types.BigIntValue(int64(task.Span.RangeID))}}, {Values: []types.Value{types.BigIntValue(10)}}}, nil
	})
	coordinator, err := NewCoordinator(Config{MaxConcurrent: 2, BatchSize: 1, BufferBatches: 1, MaxRetries: 2}, map[uint64]Endpoint{9: endpoint})
	if err != nil {
		t.Fatal(err)
	}
	spans := []ScanSpan{{RangeID: 1, ReplicaID: 9}, {RangeID: 2, ReplicaID: 9}, {RangeID: 3, ReplicaID: 9}}
	rows, err := collect(context.Background(), coordinator.Execute(context.Background(), spans))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 6 || attempts.Load() != 2 {
		t.Fatalf("rows=%+v attempts=%d", rows, attempts.Load())
	}
	if maximum.Load() > 2 {
		t.Fatalf("admission allowed %d concurrent tasks", maximum.Load())
	}
}

func TestStreamingMapHashJoinAndAggregate(t *testing.T) {
	left := streamRows([]Row{
		{Values: []types.Value{types.BigIntValue(1), mustText(t, "Alice")}},
		{Values: []types.Value{types.BigIntValue(2), mustText(t, "Bob")}},
	})
	right := streamRows([]Row{
		{Values: []types.Value{types.BigIntValue(10), types.BigIntValue(1), types.IntValue(50)}},
		{Values: []types.Value{types.BigIntValue(11), types.BigIntValue(1), types.IntValue(75)}},
		{Values: []types.Value{types.BigIntValue(12), types.BigIntValue(2), types.IntValue(20)}},
	})
	joined := HashJoin(context.Background(), left, right, 0, 1, 1)
	filtered := Map(context.Background(), joined, 1, func(row Row) (Row, bool, error) {
		amount, err := row.Values[4].AsInt64()
		return row, amount >= 50, err
	})
	rows, err := collect(context.Background(), filtered)
	if err != nil || len(rows) != 2 || rows[1].Values[4].String() != "75" {
		t.Fatalf("joined rows=%+v err=%v", rows, err)
	}

	aggregated := Aggregate(context.Background(), streamRows(rows), []AggregateSpec{
		{Kind: Count, Column: -1},
		{Kind: Sum, Column: 4, Type: types.IntType()},
	})
	result, err := collect(context.Background(), aggregated)
	if err != nil || len(result) != 1 || result[0].Values[0].String() != "2" || result[0].Values[1].String() != "125" {
		t.Fatalf("aggregate=%+v err=%v", result, err)
	}
}

func TestCancellationAndUnavailableEndpointPropagate(t *testing.T) {
	endpoint := EndpointFunc(func(ctx context.Context, task Task) ([]Row, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	coordinator, _ := NewCoordinator(Config{}, map[uint64]Endpoint{1: endpoint})
	ctx, cancel := context.WithCancel(context.Background())
	stream := coordinator.Execute(ctx, []ScanSpan{{RangeID: 1, ReplicaID: 1}})
	cancel()
	if _, err := collect(context.Background(), stream); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %v", err)
	}

	missing, _ := NewCoordinator(Config{}, map[uint64]Endpoint{1: endpoint})
	if _, err := collect(context.Background(), missing.Execute(context.Background(), []ScanSpan{{RangeID: 9, ReplicaID: 99}})); !errors.Is(err, ErrNoEndpoint) {
		t.Fatalf("missing endpoint error = %v", err)
	}
}

func TestPrimaryLookupDerivesNarrowSpan(t *testing.T) {
	table := catalog.TableDescriptor{
		ID: 7, Name: "accounts",
		Schema: codec.TableSchema{Version: 1, PrimaryKey: []uint32{1}, Columns: []codec.ColumnDescriptor{{ID: 1, Name: "id", Type: types.BigIntType()}}},
	}
	spans, err := DeriveScanSpans(plan.PhysicalPlan{Root: &plan.PhysicalNode{
		Kind: plan.ScanKind, Table: table, Access: plan.PrimaryKeyLookup, LookupValues: []types.Value{types.BigIntValue(9)},
	}}, []ranges.Descriptor{{ID: 1, ReplicaID: 1, Generation: 1}})
	if err != nil || len(spans) != 1 || len(spans[0].LogicalFrom) == 0 || len(spans[0].LogicalTo) == 0 {
		t.Fatalf("lookup spans=%+v err=%v", spans, err)
	}
}

func streamRows(rows []Row) Stream {
	batches := make(chan Batch, 1)
	errorsChannel := make(chan error)
	batches <- Batch{Rows: rows}
	close(batches)
	close(errorsChannel)
	return Stream{Batches: batches, Errors: errorsChannel}
}

func mustText(t *testing.T, value string) types.Value {
	t.Helper()
	result, err := types.TextValue(value)
	if err != nil {
		t.Fatal(err)
	}
	return result
}
