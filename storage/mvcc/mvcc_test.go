package mvcc

import (
	"errors"
	"testing"

	"pebbledb/storage/lsm"
)

func TestSnapshotReadsAndTombstones(t *testing.T) {
	manager, closeStore := openManager(t)
	defer closeStore()

	put(t, manager, "account", "v1")
	snapshot := manager.Begin()
	newer := manager.Begin()
	if err := newer.Put([]byte("account"), []byte("v2")); err != nil {
		t.Fatal(err)
	}
	if _, err := newer.Commit(); err != nil {
		t.Fatalf("commit newer: %v", err)
	}
	assertTransactionValue(t, snapshot, "account", "v1", true)
	snapshot.Rollback()

	deleted := manager.Begin()
	if err := deleted.Delete([]byte("account")); err != nil {
		t.Fatal(err)
	}
	if _, err := deleted.Commit(); err != nil {
		t.Fatalf("commit delete: %v", err)
	}
	latest := manager.Begin()
	defer latest.Rollback()
	assertTransactionValue(t, latest, "account", "", false)
}

func TestSerializablePointAndWriteConflicts(t *testing.T) {
	manager, closeStore := openManager(t)
	defer closeStore()
	put(t, manager, "a", "0")

	reader := manager.Begin()
	assertTransactionValue(t, reader, "a", "0", true)
	writer := manager.Begin()
	if err := writer.Put([]byte("a"), []byte("1")); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := reader.Put([]byte("b"), []byte("derived")); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Commit(); !errors.Is(err, ErrConflict) {
		t.Fatalf("reader commit error = %v, want conflict", err)
	}

	first, second := manager.Begin(), manager.Begin()
	_ = first.Put([]byte("same"), []byte("first"))
	_ = second.Put([]byte("same"), []byte("second"))
	if _, err := first.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := second.Commit(); !errors.Is(err, ErrConflict) {
		t.Fatalf("second writer error = %v, want conflict", err)
	}
}

func TestSerializablePhantomConflictAndReadYourWrites(t *testing.T) {
	manager, closeStore := openManager(t)
	defer closeStore()
	put(t, manager, "users/1", "Alice")

	scanner := manager.Begin()
	entries, err := scanner.Scan([]byte("users/"), []byte("users0"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("initial scan = %+v, %v", entries, err)
	}
	if err := scanner.Put([]byte("users/3"), []byte("Chandra")); err != nil {
		t.Fatal(err)
	}
	entries, err = scanner.Scan([]byte("users/"), []byte("users0"))
	if err != nil || len(entries) != 2 || string(entries[1].Value) != "Chandra" {
		t.Fatalf("read-your-writes scan = %+v, %v", entries, err)
	}

	inserter := manager.Begin()
	_ = inserter.Put([]byte("users/2"), []byte("Bob"))
	if _, err := inserter.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := scanner.Commit(); !errors.Is(err, ErrConflict) {
		t.Fatalf("scanner commit error = %v, want phantom conflict", err)
	}
}

func TestRestartRecoversVersionsAndTimestampOracle(t *testing.T) {
	directory := t.TempDir()
	store, err := lsm.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(store)
	if err != nil {
		t.Fatal(err)
	}
	put(t, manager, "key", "before")
	before := manager.Begin().ReadTimestamp()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = lsm.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	manager, err = NewManager(store)
	if err != nil {
		t.Fatal(err)
	}
	after := manager.Begin()
	defer after.Rollback()
	if after.ReadTimestamp() <= before {
		t.Fatalf("recovered timestamp %d did not advance past %d", after.ReadTimestamp(), before)
	}
	assertTransactionValue(t, after, "key", "before", true)
}

func openManager(t *testing.T) (*Manager, func()) {
	t.Helper()
	store, err := lsm.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(store)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	return manager, func() {
		if err := store.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	}
}

func put(t *testing.T, manager *Manager, key, value string) {
	t.Helper()
	transaction := manager.Begin()
	if err := transaction.Put([]byte(key), []byte(value)); err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Commit(); err != nil {
		t.Fatal(err)
	}
}

func assertTransactionValue(t *testing.T, transaction *Transaction, key, expected string, foundExpected bool) {
	t.Helper()
	value, found, err := transaction.Get([]byte(key))
	if err != nil || found != foundExpected || string(value) != expected {
		t.Fatalf("get %q = (%q, %v, %v), want (%q, %v)", key, value, found, err, expected, foundExpected)
	}
}
