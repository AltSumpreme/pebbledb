package ranges

import (
	"errors"
	"path/filepath"
	"testing"

	"pebbledb/storage/kv"
	"pebbledb/storage/lsm"
)

func TestSplitRoutesDataScansAndPersistsDescriptors(t *testing.T) {
	root := t.TempDir()
	metadata := openStore(t, filepath.Join(root, "meta"))
	leftStore := openStore(t, filepath.Join(root, "left"))
	rightStore := openStore(t, filepath.Join(root, "right"))
	router, err := Open(metadata, Replica{ID: 10, Store: leftStore})
	if err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{"a": "one", "n": "two", "z": "three"} {
		if err := router.Put([]byte(key), []byte(value)); err != nil {
			t.Fatal(err)
		}
	}
	old := router.Descriptors()[0]
	left, right, err := router.Split([]byte("m"), 2, Replica{ID: 20, Store: rightStore})
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	if string(left.End) != "m" || string(right.Start) != "m" || left.Generation != old.Generation+1 {
		t.Fatalf("unexpected descriptors: left=%+v right=%+v", left, right)
	}
	assertValue(t, router, "a", "one")
	assertValue(t, router, "n", "two")
	assertValue(t, router, "z", "three")
	if _, found, err := leftStore.Get([]byte("n")); err != nil || found {
		t.Fatalf("source copy was not cleaned up: found=%v err=%v", found, err)
	}
	entries, err := router.Scan(nil, nil)
	if err != nil || len(entries) != 3 || string(entries[0].Key) != "a" || string(entries[2].Key) != "z" {
		t.Fatalf("ordered scan = %+v, %v", entries, err)
	}
	if descriptor, _ := router.Route([]byte("m")); descriptor.ID != right.ID {
		t.Fatalf("boundary routed to range %d, want %d", descriptor.ID, right.ID)
	}
	if err := router.ApplyToRange(old.ID, old.Generation, []kv.Mutation{{Key: []byte("b"), Value: []byte("stale")}}); !errors.Is(err, ErrStaleDescriptor) {
		t.Fatalf("stale request error = %v", err)
	}

	if err := metadata.Close(); err != nil {
		t.Fatal(err)
	}
	metadata, err = lsm.Open(filepath.Join(root, "meta"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = metadata.Close() })
	reopened, err := Open(metadata, Replica{ID: 10, Store: leftStore}, Replica{ID: 20, Store: rightStore})
	if err != nil {
		t.Fatalf("reopen router: %v", err)
	}
	if len(reopened.Descriptors()) != 2 {
		t.Fatalf("reopened descriptors = %+v", reopened.Descriptors())
	}
	assertValue(t, reopened, "z", "three")
}

func TestCrossRangeBatchIsRejectedAtomically(t *testing.T) {
	metadata := openStore(t, filepath.Join(t.TempDir(), "meta"))
	left := openStore(t, filepath.Join(t.TempDir(), "left"))
	right := openStore(t, filepath.Join(t.TempDir(), "right"))
	router, err := Open(metadata, Replica{ID: 1, Store: left})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := router.Split([]byte("m"), 2, Replica{ID: 2, Store: right}); err != nil {
		t.Fatal(err)
	}
	err = router.Apply([]kv.Mutation{
		{Key: []byte("a"), Value: []byte("left")},
		{Key: []byte("z"), Value: []byte("right")},
	})
	if !errors.Is(err, ErrCrossRangeBatch) {
		t.Fatalf("batch error = %v", err)
	}
	if _, found, _ := router.Get([]byte("a")); found {
		t.Fatal("cross-range batch partially wrote left key")
	}
	if _, found, _ := router.Get([]byte("z")); found {
		t.Fatal("cross-range batch partially wrote right key")
	}
}

func TestDescriptorCorruptionAndMissingReplicaAreRejected(t *testing.T) {
	metadata := openStore(t, filepath.Join(t.TempDir(), "meta"))
	data := openStore(t, filepath.Join(t.TempDir(), "data"))
	if err := metadata.Put(descriptorKey(9), []byte("corrupt")); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(metadata, Replica{ID: 1, Store: data}); err == nil {
		t.Fatal("expected corrupt descriptor error")
	}

	cleanMeta := openStore(t, filepath.Join(t.TempDir(), "clean-meta"))
	encoded, err := encodeDescriptor(Descriptor{ID: 1, ReplicaID: 99, Generation: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := cleanMeta.Put(descriptorKey(1), encoded); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(cleanMeta, Replica{ID: 1, Store: data}); err == nil {
		t.Fatal("expected unavailable replica error")
	}
}

func openStore(t *testing.T, directory string) *lsm.Store {
	t.Helper()
	store, err := lsm.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func assertValue(t *testing.T, router *Router, key, expected string) {
	t.Helper()
	value, found, err := router.Get([]byte(key))
	if err != nil || !found || string(value) != expected {
		t.Fatalf("get %q = (%q, %v, %v), want %q", key, value, found, err, expected)
	}
}
