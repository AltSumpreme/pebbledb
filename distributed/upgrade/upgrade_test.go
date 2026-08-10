package upgrade

import (
	"errors"
	"path/filepath"
	"testing"

	"pebbledb/storage/lsm"
)

func TestRollingUpgradeRequiresEveryNodeAndRejectsOldBinary(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "cluster")
	store := openStore(t, directory)
	coordinator, err := Open(store, BaselineVersion)
	if err != nil {
		t.Fatal(err)
	}
	old := func(nodeID string) Binary { return Binary{NodeID: nodeID, Min: BaselineVersion, Max: BaselineVersion} }
	rolled := func(nodeID string) Binary { return Binary{NodeID: nodeID, Min: BaselineVersion, Max: CurrentVersion} }
	if err := coordinator.Register(old("node-a")); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Register(old("node-b")); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Begin(CurrentVersion); !errors.Is(err, ErrIncompatible) {
		t.Fatalf("begin before binary rollout = %v", err)
	}
	if err := coordinator.Register(rolled("node-a")); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Begin(CurrentVersion); !errors.Is(err, ErrIncompatible) {
		t.Fatalf("begin with one old node = %v", err)
	}
	if err := coordinator.Register(rolled("node-b")); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Begin(CurrentVersion); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Register(old("node-a")); !errors.Is(err, ErrIncompatible) {
		t.Fatalf("old binary joined pending target: %v", err)
	}
	if err := coordinator.Acknowledge("node-a", CurrentVersion); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Finalize(CurrentVersion); !errors.Is(err, ErrNotReady) {
		t.Fatalf("early finalize = %v", err)
	}
	if err := coordinator.Acknowledge("node-b", CurrentVersion); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Finalize(CurrentVersion); err != nil {
		t.Fatal(err)
	}
	status := coordinator.Status()
	if status.Active != CurrentVersion || status.Target != 0 || len(status.Nodes) != 2 {
		t.Fatalf("final status = %+v", status)
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openStore(t, directory)
	coordinator, err = Open(store, BaselineVersion)
	if err != nil {
		t.Fatal(err)
	}
	if coordinator.Status().Active != CurrentVersion {
		t.Fatalf("active version did not survive restart: %+v", coordinator.Status())
	}
	if err := coordinator.Register(old("node-c")); !errors.Is(err, ErrIncompatible) {
		t.Fatalf("old binary joined activated cluster: %v", err)
	}
}

func TestPendingUpgradeCanAbortAndNodesCanBeDecommissioned(t *testing.T) {
	coordinator, err := Open(openStore(t, t.TempDir()), BaselineVersion)
	if err != nil {
		t.Fatal(err)
	}
	for _, nodeID := range []string{"node-a", "node-b"} {
		if err := coordinator.Register(Binary{NodeID: nodeID, Min: BaselineVersion, Max: CurrentVersion}); err != nil {
			t.Fatal(err)
		}
	}
	if err := coordinator.Begin(CurrentVersion); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Acknowledge("node-a", CurrentVersion); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Abort(CurrentVersion); err != nil {
		t.Fatal(err)
	}
	status := coordinator.Status()
	if status.Active != BaselineVersion || status.Target != 0 || status.Nodes[0].Acknowledged != BaselineVersion {
		t.Fatalf("aborted status = %+v", status)
	}
	if err := coordinator.Remove("node-b"); err != nil {
		t.Fatal(err)
	}
	if err := coordinator.Remove("node-a"); err == nil {
		t.Fatal("removed final cluster node")
	}
}

func TestConcurrentCoordinatorViewsReloadDurableGeneration(t *testing.T) {
	store := openStore(t, t.TempDir())
	first, err := Open(store, BaselineVersion)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Open(store, BaselineVersion)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Register(Binary{NodeID: "node-a", Min: BaselineVersion, Max: CurrentVersion}); err != nil {
		t.Fatal(err)
	}
	if err := second.Register(Binary{NodeID: "node-b", Min: BaselineVersion, Max: CurrentVersion}); err != nil {
		t.Fatal(err)
	}
	if err := first.Begin(CurrentVersion); err != nil {
		t.Fatal(err)
	}
	if nodes := first.Status().Nodes; len(nodes) != 2 {
		t.Fatalf("generation reload lost a node: %+v", nodes)
	}
}

func TestCorruptUpgradeStateFailsClosed(t *testing.T) {
	store := openStore(t, t.TempDir())
	coordinator, err := Open(store, BaselineVersion)
	if err != nil {
		t.Fatal(err)
	}
	status := coordinator.Status()
	if status.Generation != 1 {
		t.Fatalf("bootstrap status = %+v", status)
	}
	encoded, found, err := store.Get(stateKey)
	if err != nil || !found {
		t.Fatalf("load state = %v found=%v", err, found)
	}
	encoded[len(encoded)-1] ^= 0xff
	if err := store.Put(stateKey, encoded); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(store, BaselineVersion); err == nil {
		t.Fatal("corrupt upgrade state unexpectedly opened")
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
