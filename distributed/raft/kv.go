package raft

import (
	"bytes"
	"fmt"

	"pebbledb/storage/kv"
)

type mutationCommand struct {
	Mutations []kv.Mutation `json:"mutations"`
}

type kvSnapshot struct {
	Entries []kv.Entry `json:"entries"`
}

// KVStateMachine applies committed mutation batches to an ordered store.
type KVStateMachine struct {
	store kv.BatchStore
	start []byte
	end   []byte
}

func NewKVStateMachine(store kv.BatchStore, start, end []byte) (*KVStateMachine, error) {
	if store == nil {
		return nil, fmt.Errorf("raft: KV state machine store cannot be nil")
	}
	if len(end) > 0 && bytes.Compare(start, end) > 0 {
		return nil, fmt.Errorf("raft: invalid state machine span")
	}
	return &KVStateMachine{store: store, start: cloneBytes(start), end: cloneBytes(end)}, nil
}

func (machine *KVStateMachine) Apply(command []byte) error {
	var decoded mutationCommand
	if err := decodeRecord(command, &decoded); err != nil {
		return fmt.Errorf("raft: decode KV command: %w", err)
	}
	for _, mutation := range decoded.Mutations {
		if !keyInSpan(mutation.Key, machine.start, machine.end) {
			return fmt.Errorf("raft: command key is outside state machine span")
		}
	}
	return machine.store.Apply(decoded.Mutations)
}

func (machine *KVStateMachine) Snapshot() ([]byte, error) {
	entries, err := machine.store.Scan(machine.start, machine.end)
	if err != nil {
		return nil, err
	}
	return encodeRecord(kvSnapshot{Entries: entries})
}

func (machine *KVStateMachine) Restore(snapshot []byte) error {
	var decoded kvSnapshot
	if err := decodeRecord(snapshot, &decoded); err != nil {
		return fmt.Errorf("raft: decode KV snapshot: %w", err)
	}
	current, err := machine.store.Scan(machine.start, machine.end)
	if err != nil {
		return err
	}
	mutations := make([]kv.Mutation, 0, len(current)+len(decoded.Entries))
	for _, entry := range current {
		mutations = append(mutations, kv.Mutation{Key: entry.Key, Delete: true})
	}
	for _, entry := range decoded.Entries {
		if !keyInSpan(entry.Key, machine.start, machine.end) {
			return fmt.Errorf("raft: snapshot key is outside state machine span")
		}
		mutations = append(mutations, kv.Mutation{Key: entry.Key, Value: entry.Value})
	}
	return applyChunks(machine.store, mutations)
}

// ReplicatedStore is a linearizable kv.BatchStore backed by a Raft group.
// Reads perform a quorum barrier; writes become replicated log commands.
type ReplicatedStore struct {
	group *Group
	local kv.Store
}

func NewReplicatedStore(group *Group, local kv.Store) (*ReplicatedStore, error) {
	if group == nil || local == nil {
		return nil, fmt.Errorf("raft: group and local store are required")
	}
	return &ReplicatedStore{group: group, local: local}, nil
}

func (store *ReplicatedStore) Put(key, value []byte) error {
	return store.Apply([]kv.Mutation{{Key: key, Value: value}})
}

func (store *ReplicatedStore) Delete(key []byte) error {
	return store.Apply([]kv.Mutation{{Key: key, Delete: true}})
}

func (store *ReplicatedStore) Apply(mutations []kv.Mutation) error {
	if len(mutations) == 0 {
		return nil
	}
	command, err := encodeRecord(mutationCommand{Mutations: cloneMutations(mutations)})
	if err != nil {
		return err
	}
	_, err = store.group.Propose(command)
	return err
}

func (store *ReplicatedStore) Get(key []byte) ([]byte, bool, error) {
	if err := store.group.LinearizableRead(); err != nil {
		return nil, false, err
	}
	return store.local.Get(key)
}

func (store *ReplicatedStore) Scan(start, end []byte) ([]kv.Entry, error) {
	if err := store.group.LinearizableRead(); err != nil {
		return nil, err
	}
	return store.local.Scan(start, end)
}

func applyChunks(store kv.BatchStore, mutations []kv.Mutation) error {
	const chunkSize = 1024
	for start := 0; start < len(mutations); start += chunkSize {
		end := start + chunkSize
		if end > len(mutations) {
			end = len(mutations)
		}
		if err := store.Apply(mutations[start:end]); err != nil {
			return err
		}
	}
	return nil
}

func cloneMutations(mutations []kv.Mutation) []kv.Mutation {
	result := make([]kv.Mutation, len(mutations))
	for index, mutation := range mutations {
		result[index] = kv.Mutation{Key: cloneBytes(mutation.Key), Value: cloneBytes(mutation.Value), Delete: mutation.Delete}
	}
	return result
}

func keyInSpan(key, start, end []byte) bool {
	return (len(start) == 0 || bytes.Compare(key, start) >= 0) && (len(end) == 0 || bytes.Compare(key, end) < 0)
}

func cloneBytes(value []byte) []byte { return append([]byte(nil), value...) }
