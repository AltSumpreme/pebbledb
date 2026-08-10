package query

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"sync"

	"pebbledb/codec"
	"pebbledb/types"
)

// HashJoin builds an equality hash table for the right stream and joins it with
// the left stream. Both inputs are drained concurrently to avoid exchange
// deadlocks; cancellation and upstream errors propagate.
func HashJoin(parent context.Context, left, right Stream, leftColumn, rightColumn, batchSize int) Stream {
	if batchSize <= 0 {
		batchSize = 128
	}
	ctx, cancel := context.WithCancel(parent)
	output := make(chan Batch, 2)
	errorsChannel := make(chan error, 1)
	go func() {
		defer cancel()
		defer close(output)
		defer close(errorsChannel)
		var leftRows, rightRows []Row
		var leftErr, rightErr error
		var wait sync.WaitGroup
		wait.Add(2)
		go func() { defer wait.Done(); leftRows, leftErr = collect(ctx, left) }()
		go func() { defer wait.Done(); rightRows, rightErr = collect(ctx, right) }()
		wait.Wait()
		if leftErr != nil {
			errorsChannel <- leftErr
			return
		}
		if rightErr != nil {
			errorsChannel <- rightErr
			return
		}
		hash := make(map[string][]Row)
		for _, row := range rightRows {
			if rightColumn < 0 || rightColumn >= len(row.Values) || row.Values[rightColumn].IsNull() {
				continue
			}
			key, err := codec.EncodeKey(row.Values[rightColumn])
			if err != nil {
				errorsChannel <- err
				return
			}
			hash[string(key)] = append(hash[string(key)], row)
		}
		batch := Batch{Rows: make([]Row, 0, batchSize)}
		for _, row := range leftRows {
			if leftColumn < 0 || leftColumn >= len(row.Values) || row.Values[leftColumn].IsNull() {
				continue
			}
			key, err := codec.EncodeKey(row.Values[leftColumn])
			if err != nil {
				errorsChannel <- err
				return
			}
			for _, match := range hash[string(key)] {
				values := append([]types.Value(nil), row.Values...)
				values = append(values, match.Values...)
				batch.Rows = append(batch.Rows, Row{Values: values})
				if len(batch.Rows) == batchSize {
					select {
					case output <- batch:
						batch = Batch{Rows: make([]Row, 0, batchSize)}
					case <-ctx.Done():
						return
					}
				}
			}
		}
		if len(batch.Rows) > 0 {
			select {
			case output <- batch:
			case <-ctx.Done():
			}
		}
	}()
	return Stream{Batches: output, Errors: errorsChannel}
}

type AggregateKind uint8

const (
	Count AggregateKind = iota + 1
	Sum
)

type AggregateSpec struct {
	Kind   AggregateKind
	Column int // -1 means COUNT(*)
	Type   types.Type
}

// Aggregate merges all upstream partitions into one global COUNT/SUM row.
func Aggregate(parent context.Context, input Stream, specs []AggregateSpec) Stream {
	output := make(chan Batch, 1)
	errorsChannel := make(chan error, 1)
	go func() {
		defer close(output)
		defer close(errorsChannel)
		rows, err := collect(parent, input)
		if err != nil {
			errorsChannel <- err
			return
		}
		values := make([]types.Value, 0, len(specs))
		for _, spec := range specs {
			value, err := aggregateValue(rows, spec)
			if err != nil {
				errorsChannel <- err
				return
			}
			values = append(values, value)
		}
		select {
		case output <- Batch{Rows: []Row{{Values: values}}}:
		case <-parent.Done():
		}
	}()
	return Stream{Batches: output, Errors: errorsChannel}
}

func aggregateValue(rows []Row, spec AggregateSpec) (types.Value, error) {
	if spec.Kind == Count {
		var count int64
		for _, row := range rows {
			if spec.Column < 0 || (spec.Column < len(row.Values) && !row.Values[spec.Column].IsNull()) {
				if count == math.MaxInt64 {
					return types.Value{}, fmt.Errorf("distributed query: COUNT overflow")
				}
				count++
			}
		}
		return types.BigIntValue(count), nil
	}
	if spec.Kind != Sum || !spec.Type.Valid() {
		return types.Value{}, fmt.Errorf("distributed query: invalid aggregate specification")
	}
	seen := false
	switch spec.Type.Kind() {
	case types.IntKind, types.BigIntKind:
		var sum int64
		for _, row := range rows {
			if spec.Column < 0 || spec.Column >= len(row.Values) || row.Values[spec.Column].IsNull() {
				continue
			}
			value, err := row.Values[spec.Column].AsInt64()
			if err != nil {
				return types.Value{}, err
			}
			if (value > 0 && sum > math.MaxInt64-value) || (value < 0 && sum < math.MinInt64-value) {
				return types.Value{}, fmt.Errorf("distributed query: SUM overflow")
			}
			sum, seen = sum+value, true
		}
		if !seen {
			return types.NullValue(spec.Type)
		}
		if spec.Type.Kind() == types.IntKind {
			if sum < math.MinInt32 || sum > math.MaxInt32 {
				return types.Value{}, fmt.Errorf("distributed query: INT SUM overflow")
			}
			return types.IntValue(int32(sum)), nil
		}
		return types.BigIntValue(sum), nil
	case types.DecimalKind:
		sum := new(big.Int)
		for _, row := range rows {
			if spec.Column < 0 || spec.Column >= len(row.Values) || row.Values[spec.Column].IsNull() {
				continue
			}
			coefficient, err := row.Values[spec.Column].DecimalCoefficient()
			if err != nil {
				return types.Value{}, err
			}
			sum.Add(sum, coefficient)
			seen = true
		}
		if !seen {
			return types.NullValue(spec.Type)
		}
		return types.DecimalValueFromCoefficient(spec.Type, sum)
	default:
		return types.Value{}, fmt.Errorf("distributed query: SUM does not support %s", spec.Type)
	}
}

func collect(ctx context.Context, stream Stream) ([]Row, error) {
	var rows []Row
	for {
		select {
		case batch, open := <-stream.Batches:
			if !open {
				for err := range stream.Errors {
					if err != nil {
						return nil, err
					}
				}
				return rows, nil
			}
			rows = append(rows, cloneRows(batch.Rows)...)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}
