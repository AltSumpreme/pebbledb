// Package fault provides deterministic, thread-safe failure injection around
// the internal KV contract for crash and recovery tests.
package fault

import (
	"errors"
	"fmt"
	"sync"

	"pebbledb/storage/kv"
)

type Operation string

const (
	Put    Operation = "put"
	Get    Operation = "get"
	Delete Operation = "delete"
	Scan   Operation = "scan"
	Apply  Operation = "apply"
)

var ErrInjected = errors.New("fault: injected storage failure")

type rule struct {
	remaining int
	err       error
}

// Store wraps a BatchStore. FailAfter(op, n) allows n matching calls and fails
// the next one exactly once, before the underlying store is touched.
type Store struct {
	mu       sync.Mutex
	inner    kv.BatchStore
	rules    map[Operation]rule
	counters map[Operation]uint64
}

func New(inner kv.BatchStore) (*Store, error) {
	if inner == nil {
		return nil, fmt.Errorf("fault: inner store cannot be nil")
	}
	return &Store{inner: inner, rules: make(map[Operation]rule), counters: make(map[Operation]uint64)}, nil
}

func (store *Store) FailAfter(operation Operation, successfulCalls int, err error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if successfulCalls < 0 {
		successfulCalls = 0
	}
	if err == nil {
		err = ErrInjected
	}
	store.rules[operation] = rule{remaining: successfulCalls, err: err}
}

func (store *Store) Clear() {
	store.mu.Lock()
	store.rules = make(map[Operation]rule)
	store.mu.Unlock()
}

func (store *Store) Calls(operation Operation) uint64 {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.counters[operation]
}

func (store *Store) check(operation Operation) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.counters[operation]++
	current, exists := store.rules[operation]
	if !exists {
		return nil
	}
	if current.remaining > 0 {
		current.remaining--
		store.rules[operation] = current
		return nil
	}
	delete(store.rules, operation)
	return current.err
}

func (store *Store) Put(key, value []byte) error {
	if err := store.check(Put); err != nil {
		return err
	}
	return store.inner.Put(key, value)
}

func (store *Store) Get(key []byte) ([]byte, bool, error) {
	if err := store.check(Get); err != nil {
		return nil, false, err
	}
	return store.inner.Get(key)
}

func (store *Store) Delete(key []byte) error {
	if err := store.check(Delete); err != nil {
		return err
	}
	return store.inner.Delete(key)
}

func (store *Store) Scan(start, end []byte) ([]kv.Entry, error) {
	if err := store.check(Scan); err != nil {
		return nil, err
	}
	return store.inner.Scan(start, end)
}

func (store *Store) Apply(mutations []kv.Mutation) error {
	if err := store.check(Apply); err != nil {
		return err
	}
	return store.inner.Apply(mutations)
}
