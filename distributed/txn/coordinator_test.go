package txn

import (
	"context"
	"errors"
	"testing"

	"pebbledb/storage/fault"
	"pebbledb/storage/kv"
	"pebbledb/storage/lsm"
)

func TestCommitAppliesEveryParticipant(t *testing.T) {
	coordinatorStore := openStore(t, t.TempDir())
	left := newParticipant(t, "left", openStore(t, t.TempDir()))
	right := newParticipant(t, "right", openStore(t, t.TempDir()))
	coordinator, err := NewCoordinator(coordinatorStore)
	if err != nil {
		t.Fatal(err)
	}

	err = coordinator.Commit(context.Background(), "txn-1", []Batch{
		{Participant: left, Mutations: []kv.Mutation{{Key: []byte("users/1"), Value: []byte("alice")}}},
		{Participant: right, Mutations: []kv.Mutation{{Key: []byte("orders/9"), Value: []byte("42")}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	assertValue(t, left.store, "users/1", "alice", true)
	assertValue(t, right.store, "orders/9", "42", true)
	assertNoDecisions(t, coordinatorStore)
}

func TestRecoverRollsForwardDurableCommitAfterRestart(t *testing.T) {
	coordinatorDirectory, leftDirectory, rightDirectory := t.TempDir(), t.TempDir(), t.TempDir()
	coordinatorStore := openStore(t, coordinatorDirectory)
	leftStore := openStore(t, leftDirectory)
	rightStore := openStore(t, rightDirectory)
	injected, err := fault.New(rightStore)
	if err != nil {
		t.Fatal(err)
	}
	injected.FailAfter(fault.Apply, 1, nil) // prepare succeeds; commit application fails
	left := newParticipant(t, "left", leftStore)
	right := newParticipant(t, "right", injected)
	coordinator, _ := NewCoordinator(coordinatorStore)

	err = coordinator.Commit(context.Background(), "txn-recover", []Batch{
		{Participant: left, Mutations: []kv.Mutation{{Key: []byte("left-key"), Value: []byte("left-value")}}},
		{Participant: right, Mutations: []kv.Mutation{{Key: []byte("right-key"), Value: []byte("right-value")}}},
	})
	if !errors.Is(err, ErrInDoubt) {
		t.Fatalf("commit error = %v, want ErrInDoubt", err)
	}
	assertValue(t, leftStore, "left-key", "left-value", true)
	assertValue(t, rightStore, "right-key", "", false)

	if err := coordinatorStore.Close(); err != nil {
		t.Fatal(err)
	}
	if err := leftStore.Close(); err != nil {
		t.Fatal(err)
	}
	if err := rightStore.Close(); err != nil {
		t.Fatal(err)
	}

	coordinatorStore = openStore(t, coordinatorDirectory)
	leftStore = openStore(t, leftDirectory)
	rightStore = openStore(t, rightDirectory)
	coordinator, _ = NewCoordinator(coordinatorStore)
	left = newParticipant(t, "left", leftStore)
	right = newParticipant(t, "right", rightStore)
	if err := coordinator.Recover(context.Background(), map[string]*Participant{"left": left, "right": right}); err != nil {
		t.Fatal(err)
	}
	assertValue(t, leftStore, "left-key", "left-value", true)
	assertValue(t, rightStore, "right-key", "right-value", true)
	assertNoDecisions(t, coordinatorStore)
}

func TestPrepareFailureAbortsPreparedParticipants(t *testing.T) {
	coordinatorStore := openStore(t, t.TempDir())
	leftStore := openStore(t, t.TempDir())
	rightStore := openStore(t, t.TempDir())
	injected, err := fault.New(rightStore)
	if err != nil {
		t.Fatal(err)
	}
	injected.FailAfter(fault.Apply, 0, nil)
	left := newParticipant(t, "left", leftStore)
	right := newParticipant(t, "right", injected)
	coordinator, _ := NewCoordinator(coordinatorStore)

	err = coordinator.Commit(context.Background(), "txn-abort", []Batch{
		{Participant: left, Mutations: []kv.Mutation{{Key: []byte("left-key"), Value: []byte("left-value")}}},
		{Participant: right, Mutations: []kv.Mutation{{Key: []byte("right-key"), Value: []byte("right-value")}}},
	})
	if !errors.Is(err, fault.ErrInjected) || errors.Is(err, ErrInDoubt) {
		t.Fatalf("commit error = %v, want injected prepare failure", err)
	}
	assertValue(t, leftStore, "left-key", "", false)
	assertValue(t, rightStore, "right-key", "", false)
	assertNoDecisions(t, coordinatorStore)
}

func TestAmbiguousDecisionWriteIsRecoveredWithoutUnsafeAbort(t *testing.T) {
	innerCoordinatorStore := openStore(t, t.TempDir())
	injected, err := fault.New(innerCoordinatorStore)
	if err != nil {
		t.Fatal(err)
	}
	injected.FailAfter(fault.Apply, 1, nil) // PREPARING persists, COMMIT attempt fails
	left := newParticipant(t, "left", openStore(t, t.TempDir()))
	right := newParticipant(t, "right", openStore(t, t.TempDir()))
	coordinator, _ := NewCoordinator(injected)

	err = coordinator.Commit(context.Background(), "txn-ambiguous", []Batch{
		{Participant: left, Mutations: []kv.Mutation{{Key: []byte("left-key"), Value: []byte("left-value")}}},
		{Participant: right, Mutations: []kv.Mutation{{Key: []byte("right-key"), Value: []byte("right-value")}}},
	})
	if !errors.Is(err, ErrInDoubt) {
		t.Fatalf("commit error = %v, want ErrInDoubt", err)
	}
	injected.Clear()
	if err := coordinator.Recover(context.Background(), map[string]*Participant{"left": left, "right": right}); err != nil {
		t.Fatal(err)
	}
	assertValue(t, left.store, "left-key", "", false)
	assertValue(t, right.store, "right-key", "", false)
	assertNoDecisions(t, innerCoordinatorStore)
}

func TestCommitDecisionCannotChangeToAbort(t *testing.T) {
	store := openStore(t, t.TempDir())
	coordinator, _ := NewCoordinator(store)
	record := decisionRecord{TransactionID: "fixed", Decision: preparing, Participants: []string{"left", "right"}}
	if err := coordinator.putDecision(record); err != nil {
		t.Fatal(err)
	}
	record.Decision = commitDecision
	if err := coordinator.putDecision(record); err != nil {
		t.Fatal(err)
	}
	record.Decision = abortDecision
	if err := coordinator.putDecision(record); !errors.Is(err, ErrDuplicateID) {
		t.Fatalf("decision transition error = %v, want ErrDuplicateID", err)
	}
}

func TestRecoveryRejectsCorruptDecisionWithoutDeletingIt(t *testing.T) {
	store := openStore(t, t.TempDir())
	record := decisionRecord{TransactionID: "corrupt", Decision: commitDecision, Participants: []string{"left", "right"}}
	encoded, err := encode(record)
	if err != nil {
		t.Fatal(err)
	}
	encoded[len(encoded)-1] ^= 0xff
	if err := store.Apply([]kv.Mutation{{Key: decisionKey(record.TransactionID), Value: encoded}}); err != nil {
		t.Fatal(err)
	}
	coordinator, _ := NewCoordinator(store)
	if err := coordinator.Recover(context.Background(), nil); err == nil {
		t.Fatal("corrupt decision unexpectedly recovered")
	}
	entries, err := store.Scan(decisionPrefix, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("corrupt decision count = %d, want 1", len(entries))
	}
}

func openStore(t *testing.T, directory string) *lsm.Store {
	t.Helper()
	store, err := lsm.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return store
}

func newParticipant(t *testing.T, id string, store kv.BatchStore) *Participant {
	t.Helper()
	participant, err := NewParticipant(id, store)
	if err != nil {
		t.Fatal(err)
	}
	return participant
}

func assertValue(t *testing.T, store kv.BatchStore, key, expected string, expectedFound bool) {
	t.Helper()
	value, found, err := store.Get([]byte(key))
	if err != nil {
		t.Fatal(err)
	}
	if found != expectedFound || string(value) != expected {
		t.Fatalf("get %q = (%q, %v), want (%q, %v)", key, value, found, expected, expectedFound)
	}
}

func assertNoDecisions(t *testing.T, store kv.BatchStore) {
	t.Helper()
	entries, err := store.Scan(decisionPrefix, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if len(entry.Key) >= len(decisionPrefix) && string(entry.Key[:len(decisionPrefix)]) == string(decisionPrefix) {
			t.Fatalf("unresolved decision %x", entry.Key)
		}
	}
}
