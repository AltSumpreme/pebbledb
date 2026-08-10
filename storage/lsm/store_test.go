package lsm

import (
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"pebbledb/storage/kv"
)

func TestPutGetDeleteAndCopies(t *testing.T) {
	store := openTestStore(t, Options{})

	key := []byte("table/1/row/1")
	value := []byte("Reuben")
	if err := store.Put(key, value); err != nil {
		t.Fatalf("put: %v", err)
	}
	key[0] = 'X'
	value[0] = 'X'

	got, found, err := store.Get([]byte("table/1/row/1"))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !found || string(got) != "Reuben" {
		t.Fatalf("got (%q, %v), want (Reuben, true)", got, found)
	}
	got[0] = 'X'
	gotAgain, found, err := store.Get([]byte("table/1/row/1"))
	if err != nil || !found || string(gotAgain) != "Reuben" {
		t.Fatalf("store exposed mutable value: got (%q, %v, %v)", gotAgain, found, err)
	}

	if err := store.Delete([]byte("table/1/row/1")); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, found, err := store.Get([]byte("table/1/row/1")); err != nil || found {
		t.Fatalf("deleted key returned (found=%v, err=%v)", found, err)
	}
	if err := store.Put([]byte("empty"), nil); err != nil {
		t.Fatalf("put empty value: %v", err)
	}
	got, found, err = store.Get([]byte("empty"))
	if err != nil || !found || len(got) != 0 {
		t.Fatalf("empty value got (%v, %v, %v)", got, found, err)
	}
}

func TestWALRecoveryWithoutCleanClose(t *testing.T) {
	directory := t.TempDir()
	store, err := Open(directory)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.Put([]byte("catalog/table/users"), []byte("schema")); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := store.Delete([]byte("catalog/table/old")); err != nil {
		t.Fatalf("delete: %v", err)
	}
	simulateCrash(t, store)

	recovered, err := Open(directory)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	t.Cleanup(func() { _ = recovered.Close() })
	assertValue(t, recovered, "catalog/table/users", "schema", true)
	assertValue(t, recovered, "catalog/table/old", "", false)
}

func TestAtomicBatchRecovery(t *testing.T) {
	directory := t.TempDir()
	store, err := Open(directory)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.Apply([]kv.Mutation{
		{Key: []byte("row/1"), Value: []byte("one")},
		{Key: []byte("row/2"), Value: []byte("two")},
		{Key: []byte("row/old"), Delete: true},
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	simulateCrash(t, store)

	recovered, err := Open(directory)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	t.Cleanup(func() { _ = recovered.Close() })
	assertValue(t, recovered, "row/1", "one", true)
	assertValue(t, recovered, "row/2", "two", true)
	assertValue(t, recovered, "row/old", "", false)
}

func TestIncompleteAtomicBatchReplaysNothing(t *testing.T) {
	directory := t.TempDir()
	store, err := Open(directory)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.Apply([]kv.Mutation{
		{Key: []byte("row/1"), Value: []byte("one")},
		{Key: []byte("row/2"), Value: []byte("two")},
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	simulateCrash(t, store)

	walPath := filepath.Join(directory, walFilename)
	info, err := os.Stat(walPath)
	if err != nil {
		t.Fatalf("stat WAL: %v", err)
	}
	if err := os.Truncate(walPath, info.Size()-3); err != nil {
		t.Fatalf("truncate batch: %v", err)
	}
	recovered, err := Open(directory)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	t.Cleanup(func() { _ = recovered.Close() })
	assertValue(t, recovered, "row/1", "", false)
	assertValue(t, recovered, "row/2", "", false)
}

func TestWALIgnoresIncompleteTrailingRecord(t *testing.T) {
	directory := t.TempDir()
	store, err := Open(directory)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.Put([]byte("durable"), []byte("value")); err != nil {
		t.Fatalf("put: %v", err)
	}
	simulateCrash(t, store)

	walPath := filepath.Join(directory, walFilename)
	wal, err := os.OpenFile(walPath, os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		t.Fatalf("open WAL for partial append: %v", err)
	}
	if _, err := wal.Write([]byte{0x20, 0x00, 0x00}); err != nil {
		_ = wal.Close()
		t.Fatalf("append partial WAL header: %v", err)
	}
	if err := wal.Close(); err != nil {
		t.Fatalf("close partial WAL: %v", err)
	}

	recovered, err := Open(directory)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	t.Cleanup(func() { _ = recovered.Close() })
	assertValue(t, recovered, "durable", "value", true)

	info, err := os.Stat(walPath)
	if err != nil {
		t.Fatalf("stat WAL: %v", err)
	}
	if info.Size() <= int64(walHeaderSize) {
		t.Fatalf("valid WAL record was unexpectedly removed; size=%d", info.Size())
	}
}

func TestWALRejectsChecksumCorruption(t *testing.T) {
	directory := t.TempDir()
	store, err := Open(directory)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.Put([]byte("key"), []byte("value")); err != nil {
		t.Fatalf("put: %v", err)
	}
	simulateCrash(t, store)

	walPath := filepath.Join(directory, walFilename)
	contents, err := os.ReadFile(walPath)
	if err != nil {
		t.Fatalf("read WAL: %v", err)
	}
	contents[len(contents)-1] ^= 0xff
	if err := os.WriteFile(walPath, contents, 0644); err != nil {
		t.Fatalf("corrupt WAL: %v", err)
	}
	if _, err := Open(directory); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("expected checksum error, got %v", err)
	}
}

func TestSSTableRejectsChecksumCorruption(t *testing.T) {
	directory := t.TempDir()
	store, err := Open(directory)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.Put([]byte("key"), []byte("value")); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := store.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	tablePath := store.tables[0].path
	simulateCrash(t, store)

	contents, err := os.ReadFile(tablePath)
	if err != nil {
		t.Fatalf("read SSTable: %v", err)
	}
	contents[len(contents)-1] ^= 0xff
	if err := os.WriteFile(tablePath, contents, 0644); err != nil {
		t.Fatalf("corrupt SSTable: %v", err)
	}
	if _, err := Open(directory); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("expected checksum error, got %v", err)
	}
}

func TestFlushRestartAndOrderedScan(t *testing.T) {
	directory := t.TempDir()
	store, err := Open(directory)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for key, value := range map[string]string{
		"row/d": "4",
		"row/b": "2",
		"row/a": "1",
		"row/c": "3",
	} {
		if err := store.Put([]byte(key), []byte(value)); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}
	if err := store.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if err := store.Put([]byte("row/b"), []byte("updated")); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := store.Delete([]byte("row/c")); err != nil {
		t.Fatalf("delete: %v", err)
	}

	entries, err := store.Scan([]byte("row/b"), []byte("row/z"))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	assertEntries(t, entries, []Entry{
		{Key: []byte("row/b"), Value: []byte("updated")},
		{Key: []byte("row/d"), Value: []byte("4")},
	})
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := Open(directory)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	assertValue(t, reopened, "row/b", "updated", true)
	assertValue(t, reopened, "row/c", "", false)
	entries, err = reopened.Scan(nil, nil)
	if err != nil {
		t.Fatalf("scan after reopen: %v", err)
	}
	assertEntries(t, entries, []Entry{
		{Key: []byte("row/a"), Value: []byte("1")},
		{Key: []byte("row/b"), Value: []byte("updated")},
		{Key: []byte("row/d"), Value: []byte("4")},
	})
}

func TestAutomaticFlushAndCompaction(t *testing.T) {
	directory := t.TempDir()
	store, err := Open(directory, Options{MemtableSizeBytes: 1})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	if err := store.Put([]byte("a"), []byte("old")); err != nil {
		t.Fatalf("put old: %v", err)
	}
	if err := store.Put([]byte("a"), []byte("new")); err != nil {
		t.Fatalf("put new: %v", err)
	}
	if err := store.Put([]byte("b"), []byte("value")); err != nil {
		t.Fatalf("put b: %v", err)
	}
	if err := store.Delete([]byte("b")); err != nil {
		t.Fatalf("delete b: %v", err)
	}
	if stats := store.Stats(); stats.SSTables < 4 || stats.MemtableEntries != 0 {
		t.Fatalf("automatic flush did not create expected tables: %+v", stats)
	}
	if err := store.Compact(); err != nil {
		t.Fatalf("compact: %v", err)
	}
	if stats := store.Stats(); stats.SSTables != 1 {
		t.Fatalf("got %d SSTables after compaction, want 1", stats.SSTables)
	}
	assertValue(t, store, "a", "new", true)
	assertValue(t, store, "b", "", false)
}

func TestConcurrentAccess(t *testing.T) {
	store := openTestStore(t, Options{})
	var wait sync.WaitGroup
	for i := byte(0); i < 32; i++ {
		wait.Add(1)
		go func(index byte) {
			defer wait.Done()
			key := []byte{'k', index}
			value := []byte{'v', index}
			if err := store.Put(key, value); err != nil {
				t.Errorf("put %d: %v", index, err)
				return
			}
			got, found, err := store.Get(key)
			if err != nil || !found || string(got) != string(value) {
				t.Errorf("get %d = (%v, %v, %v)", index, got, found, err)
			}
		}(i)
	}
	wait.Wait()
}

func TestInputValidationAndClosedStore(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := store.Put(nil, []byte("value")); !errors.Is(err, ErrEmptyKey) {
		t.Fatalf("empty key error = %v", err)
	}
	if _, err := store.Scan([]byte("z"), []byte("a")); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("invalid range error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := store.Put([]byte("key"), []byte("value")); !errors.Is(err, ErrClosed) {
		t.Fatalf("put after close error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestRandomizedOperationsSurviveFlushCompactAndRestart(t *testing.T) {
	directory := t.TempDir()
	store, err := Open(directory, Options{MemtableSizeBytes: 256})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	random := rand.New(rand.NewSource(42))
	model := make(map[string]string)
	for operation := 1; operation <= 300; operation++ {
		key := fmt.Sprintf("table/7/row/%02d", random.Intn(24))
		if random.Intn(4) == 0 {
			if err := store.Delete([]byte(key)); err != nil {
				t.Fatalf("operation %d delete: %v", operation, err)
			}
			delete(model, key)
		} else {
			value := fmt.Sprintf("value-%d", operation)
			if err := store.Put([]byte(key), []byte(value)); err != nil {
				t.Fatalf("operation %d put: %v", operation, err)
			}
			model[key] = value
		}

		if operation%37 == 0 {
			if err := store.Flush(); err != nil {
				t.Fatalf("operation %d flush: %v", operation, err)
			}
		}
		if operation%89 == 0 {
			if err := store.Compact(); err != nil {
				t.Fatalf("operation %d compact: %v", operation, err)
			}
		}
		if operation%113 == 0 {
			if err := store.Close(); err != nil {
				t.Fatalf("operation %d close: %v", operation, err)
			}
			store, err = Open(directory, Options{MemtableSizeBytes: 256})
			if err != nil {
				t.Fatalf("operation %d reopen: %v", operation, err)
			}
		}
	}

	entries, err := store.Scan(nil, nil)
	if err != nil {
		t.Fatalf("final scan: %v", err)
	}
	if len(entries) != len(model) {
		t.Fatalf("final entry count = %d, model count = %d", len(entries), len(model))
	}
	for _, entry := range entries {
		if expected, ok := model[string(entry.Key)]; !ok || string(entry.Value) != expected {
			t.Fatalf("unexpected final entry (%q, %q), model value %q exists=%v", entry.Key, entry.Value, expected, ok)
		}
	}
}

func openTestStore(t *testing.T, options Options) *Store {
	t.Helper()
	store, err := Open(t.TempDir(), options)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})
	return store
}

func simulateCrash(t *testing.T, store *Store) {
	t.Helper()
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.wal.close(); err != nil {
		t.Fatalf("close WAL for simulated crash: %v", err)
	}
	closeSSTables(store.tables)
	store.closed = true
}

func assertValue(t *testing.T, store *Store, key, expected string, expectedFound bool) {
	t.Helper()
	value, found, err := store.Get([]byte(key))
	if err != nil {
		t.Fatalf("get %q: %v", key, err)
	}
	if found != expectedFound || string(value) != expected {
		t.Fatalf("get %q = (%q, %v), want (%q, %v)", key, value, found, expected, expectedFound)
	}
}

func assertEntries(t *testing.T, actual, expected []Entry) {
	t.Helper()
	if len(actual) != len(expected) {
		t.Fatalf("entry count = %d, want %d: %+v", len(actual), len(expected), actual)
	}
	for i := range expected {
		if string(actual[i].Key) != string(expected[i].Key) || string(actual[i].Value) != string(expected[i].Value) {
			t.Fatalf("entry %d = (%q, %q), want (%q, %q)", i, actual[i].Key, actual[i].Value, expected[i].Key, expected[i].Value)
		}
	}
}
