package query

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"pebbledb/types"
)

var (
	ErrNoEndpoint = errors.New("distributed query: replica endpoint is unavailable")
	ErrRetryable  = errors.New("distributed query: retryable processor failure")
)

// Row is the transport-neutral result of a processor. Key is an optional
// resume key; Values are typed SQL cells.
type Row struct {
	Key    []byte
	Values []types.Value
}

type Batch struct{ Rows []Row }

type Task struct {
	Span    ScanSpan
	Attempt int
}

// Endpoint executes a range-local processor task. Network RPC can implement
// this interface without changing coordinator scheduling.
type Endpoint interface {
	Execute(context.Context, Task) ([]Row, error)
}

type EndpointFunc func(context.Context, Task) ([]Row, error)

func (function EndpointFunc) Execute(ctx context.Context, task Task) ([]Row, error) {
	return function(ctx, task)
}

type Config struct {
	MaxConcurrent int
	BatchSize     int
	BufferBatches int
	MaxRetries    int
}

func (config Config) normalized() Config {
	if config.MaxConcurrent <= 0 {
		config.MaxConcurrent = 8
	}
	if config.BatchSize <= 0 {
		config.BatchSize = 128
	}
	if config.BufferBatches <= 0 {
		config.BufferBatches = 4
	}
	if config.MaxRetries < 0 {
		config.MaxRetries = 0
	}
	return config
}

// Coordinator schedules remote tasks under an admission limit and streams
// bounded batches. It retries only failures explicitly wrapping ErrRetryable,
// before any rows from that task have entered the exchange.
type Coordinator struct {
	config    Config
	endpoints map[uint64]Endpoint
	admission chan struct{}
}

func NewCoordinator(config Config, endpoints map[uint64]Endpoint) (*Coordinator, error) {
	config = config.normalized()
	if len(endpoints) == 0 {
		return nil, fmt.Errorf("distributed query: at least one endpoint is required")
	}
	copyEndpoints := make(map[uint64]Endpoint, len(endpoints))
	for id, endpoint := range endpoints {
		if id == 0 || endpoint == nil {
			return nil, fmt.Errorf("distributed query: endpoint ID and implementation are required")
		}
		copyEndpoints[id] = endpoint
	}
	return &Coordinator{config: config, endpoints: copyEndpoints, admission: make(chan struct{}, config.MaxConcurrent)}, nil
}

// Stream is a bounded exchange. Consumers must drain Batches and then inspect
// Errors; both channels close when all processors stop.
type Stream struct {
	Batches <-chan Batch
	Errors  <-chan error
}

func (coordinator *Coordinator) Execute(parent context.Context, spans []ScanSpan) Stream {
	ctx, cancel := context.WithCancel(parent)
	batches := make(chan Batch, coordinator.config.BufferBatches)
	errorsChannel := make(chan error, 1)
	var workers sync.WaitGroup
	var firstError sync.Once
	fail := func(err error) {
		firstError.Do(func() {
			errorsChannel <- err
			cancel()
		})
	}
	for _, span := range spans {
		span := span
		workers.Add(1)
		go func() {
			defer workers.Done()
			select {
			case coordinator.admission <- struct{}{}:
				defer func() { <-coordinator.admission }()
			case <-ctx.Done():
				return
			}
			endpoint := coordinator.endpoints[span.ReplicaID]
			if endpoint == nil {
				fail(fmt.Errorf("%w: replica %d", ErrNoEndpoint, span.ReplicaID))
				return
			}
			var rows []Row
			var err error
			for attempt := 0; attempt <= coordinator.config.MaxRetries; attempt++ {
				rows, err = endpoint.Execute(ctx, Task{Span: span, Attempt: attempt})
				if err == nil || !errors.Is(err, ErrRetryable) || attempt == coordinator.config.MaxRetries {
					break
				}
			}
			if err != nil {
				if parent.Err() != nil {
					fail(parent.Err())
				} else if !errors.Is(err, context.Canceled) {
					fail(fmt.Errorf("range %d processor: %w", span.RangeID, err))
				}
				return
			}
			for start := 0; start < len(rows); start += coordinator.config.BatchSize {
				end := start + coordinator.config.BatchSize
				if end > len(rows) {
					end = len(rows)
				}
				batch := Batch{Rows: cloneRows(rows[start:end])}
				select {
				case batches <- batch:
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	go func() {
		workers.Wait()
		if err := parent.Err(); err != nil {
			fail(err)
		}
		cancel()
		close(batches)
		close(errorsChannel)
	}()
	return Stream{Batches: batches, Errors: errorsChannel}
}

// Map is a local streaming processor used for filters and projections. Returning
// keep=false filters a row; returned errors cancel the downstream flow.
func Map(parent context.Context, input Stream, buffer int, function func(Row) (Row, bool, error)) Stream {
	if buffer <= 0 {
		buffer = 1
	}
	ctx, cancel := context.WithCancel(parent)
	output := make(chan Batch, buffer)
	errorsChannel := make(chan error, 1)
	go func() {
		defer cancel()
		defer close(output)
		defer close(errorsChannel)
		for {
			select {
			case batch, open := <-input.Batches:
				if !open {
					for err := range input.Errors {
						if err != nil {
							errorsChannel <- err
						}
					}
					return
				}
				mapped := Batch{Rows: make([]Row, 0, len(batch.Rows))}
				for _, row := range batch.Rows {
					result, keep, err := function(row)
					if err != nil {
						errorsChannel <- err
						return
					}
					if keep {
						mapped.Rows = append(mapped.Rows, result)
					}
				}
				if len(mapped.Rows) > 0 {
					select {
					case output <- mapped:
					case <-ctx.Done():
						return
					}
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return Stream{Batches: output, Errors: errorsChannel}
}

func cloneRows(rows []Row) []Row {
	result := make([]Row, len(rows))
	for index, row := range rows {
		result[index] = Row{Key: append([]byte(nil), row.Key...), Values: append([]types.Value(nil), row.Values...)}
	}
	return result
}
