package fault

import (
	"errors"
	"testing"

	"pebbledb/storage/kv"
	"pebbledb/storage/lsm"
)

func TestDeterministicFailureOccursBeforeAtomicBatch(t *testing.T) {
	inner, err := lsm.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer inner.Close()
	store, _ := New(inner)
	store.FailAfter(Apply, 1, nil)
	first := []kv.Mutation{{Key: []byte("a"), Value: []byte("one")}}
	if err := store.Apply(first); err != nil {
		t.Fatal(err)
	}
	second := []kv.Mutation{{Key: []byte("b"), Value: []byte("two")}, {Key: []byte("c"), Value: []byte("three")}}
	if err := store.Apply(second); !errors.Is(err, ErrInjected) {
		t.Fatalf("injected error=%v", err)
	}
	for _, key := range []string{"b", "c"} {
		if _, found, err := inner.Get([]byte(key)); err != nil || found {
			t.Fatalf("failed batch wrote %q: found=%v err=%v", key, found, err)
		}
	}
	if err := store.Apply(second); err != nil {
		t.Fatalf("one-shot failure did not clear: %v", err)
	}
	if got := store.Calls(Apply); got != 3 {
		t.Fatalf("Apply calls=%d", got)
	}
}
