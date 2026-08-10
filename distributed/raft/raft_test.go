package raft

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"pebbledb/storage/kv"
	"pebbledb/storage/lsm"
)

func TestKVStateMachineReplicatesAndRestoresDisjointSpans(t *testing.T) {
	store, err := lsm.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	machine, err := NewKVStateMachineWithSpans(store,
		KeySpan{Start: []byte("a"), End: []byte("c")},
		KeySpan{Start: []byte("x"), End: []byte("z")},
	)
	if err != nil {
		t.Fatal(err)
	}
	command, _ := encodeRecord(mutationCommand{Mutations: []kv.Mutation{
		{Key: []byte("b"), Value: []byte("left")},
		{Key: []byte("y"), Value: []byte("right")},
	}})
	if err := machine.Apply(command); err != nil {
		t.Fatal(err)
	}
	snapshot, err := machine.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	outside, _ := encodeRecord(mutationCommand{Mutations: []kv.Mutation{{Key: []byte("m"), Value: []byte("outside")}}})
	if err := machine.Apply(outside); err == nil {
		t.Fatal("out-of-span command unexpectedly applied")
	}
	if err := store.Put([]byte("b"), []byte("changed")); err != nil {
		t.Fatal(err)
	}
	if err := machine.Restore(snapshot); err != nil {
		t.Fatal(err)
	}
	for key, expected := range map[string]string{"b": "left", "y": "right"} {
		value, found, err := store.Get([]byte(key))
		if err != nil || !found || string(value) != expected {
			t.Fatalf("restored %s = (%q, %v, %v)", key, value, found, err)
		}
	}
}

func TestElectionQuorumReplicationPartitionAndConflictRepair(t *testing.T) {
	cluster := newTestCluster(t, 3)
	if err := cluster.group.Elect(1); err != nil {
		t.Fatalf("elect node 1: %v", err)
	}
	store, _ := NewReplicatedStore(cluster.group, cluster.nodes[1].data)
	if err := store.Put([]byte("account/a"), []byte("100")); err != nil {
		t.Fatalf("replicated put: %v", err)
	}
	cluster.assertAll(t, "account/a", "100", []uint64{1, 2, 3})

	_ = cluster.group.SetPartition(1, 2, true)
	_ = cluster.group.SetPartition(1, 3, true)
	if err := store.Put([]byte("isolated"), []byte("must-not-commit")); !errors.Is(err, ErrNoQuorum) {
		t.Fatalf("isolated write error = %v", err)
	}
	if _, found, _ := cluster.nodes[1].data.Get([]byte("isolated")); found {
		t.Fatal("uncommitted leader entry reached its state machine")
	}

	_ = cluster.group.SetPartition(1, 2, false)
	_ = cluster.group.SetPartition(1, 3, false)
	if err := cluster.group.Elect(2); err != nil {
		t.Fatalf("elect node 2: %v", err)
	}
	store, _ = NewReplicatedStore(cluster.group, cluster.nodes[2].data)
	if err := store.Put([]byte("recovered"), []byte("yes")); err != nil {
		t.Fatalf("write after election: %v", err)
	}
	cluster.assertAll(t, "recovered", "yes", []uint64{1, 2, 3})
	if _, found, _ := cluster.nodes[1].data.Get([]byte("isolated")); found {
		t.Fatal("conflicting uncommitted entry survived log repair")
	}

	if err := cluster.group.SetAvailable(3, false); err != nil {
		t.Fatal(err)
	}
	if err := store.Put([]byte("quorum"), []byte("two-of-three")); err != nil {
		t.Fatalf("quorum write: %v", err)
	}
	if _, found, _ := cluster.nodes[3].data.Get([]byte("quorum")); found {
		t.Fatal("offline follower unexpectedly applied a write")
	}
	_ = cluster.group.SetAvailable(3, true)
	if err := cluster.group.LinearizableRead(); err != nil {
		t.Fatalf("catch-up barrier: %v", err)
	}
	cluster.assertAll(t, "quorum", "two-of-three", []uint64{1, 2, 3})
}

func TestSnapshotInstallationAndNodeRecovery(t *testing.T) {
	cluster := newTestCluster(t, 3)
	if err := cluster.group.Elect(1); err != nil {
		t.Fatal(err)
	}
	store, _ := NewReplicatedStore(cluster.group, cluster.nodes[1].data)
	_ = cluster.group.SetAvailable(3, false)
	for index := 1; index <= 4; index++ {
		if err := store.Put([]byte(fmt.Sprintf("key/%d", index)), []byte(fmt.Sprintf("value/%d", index))); err != nil {
			t.Fatal(err)
		}
	}
	snapshotIndex, err := cluster.group.Snapshot()
	if err != nil || snapshotIndex != 4 {
		t.Fatalf("snapshot = (%d, %v), want index 4", snapshotIndex, err)
	}
	_ = cluster.group.SetAvailable(3, true)
	if err := cluster.group.LinearizableRead(); err != nil {
		t.Fatalf("install snapshot: %v", err)
	}
	cluster.assertAll(t, "key/4", "value/4", []uint64{1, 2, 3})
	if status := cluster.nodes[3].node.Status(); status.LastApplied != 4 || status.CommitIndex != 4 {
		t.Fatalf("snapshot follower status = %+v", status)
	}

	member := cluster.nodes[1]
	if err := member.journal.Close(); err != nil {
		t.Fatal(err)
	}
	if err := member.data.Close(); err != nil {
		t.Fatal(err)
	}
	journal, err := lsm.Open(member.journalPath)
	if err != nil {
		t.Fatal(err)
	}
	data, err := lsm.Open(member.dataPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close(); _ = data.Close() })
	machine, _ := NewKVStateMachine(data, nil, nil)
	reopened, err := OpenNode(77, 1, journal, machine, []uint64{1, 2, 3})
	if err != nil {
		t.Fatalf("reopen node: %v", err)
	}
	if status := reopened.Status(); status.CommitIndex != 4 || status.LastApplied != 4 {
		t.Fatalf("recovered status = %+v", status)
	}
	value, found, err := data.Get([]byte("key/3"))
	if err != nil || !found || string(value) != "value/3" {
		t.Fatalf("recovered state = (%q, %v, %v)", value, found, err)
	}
}

func TestMembershipChangesAreReplicated(t *testing.T) {
	cluster := newTestCluster(t, 3)
	if err := cluster.group.Elect(1); err != nil {
		t.Fatal(err)
	}
	newMember := cluster.newNode(t, 4, []uint64{1, 2, 3, 4})
	if err := cluster.group.AddMember(newMember.node); err != nil {
		t.Fatalf("add member: %v", err)
	}
	if got := cluster.group.Members(); len(got) != 4 || got[3] != 4 {
		t.Fatalf("membership after add = %v", got)
	}
	store, _ := NewReplicatedStore(cluster.group, cluster.nodes[1].data)
	if err := store.Put([]byte("after-add"), []byte("replicated")); err != nil {
		t.Fatal(err)
	}
	cluster.assertAll(t, "after-add", "replicated", []uint64{1, 2, 3, 4})

	if err := cluster.group.RemoveMember(3); err != nil {
		t.Fatalf("remove member: %v", err)
	}
	if got := cluster.group.Members(); len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 4 {
		t.Fatalf("membership after removal = %v", got)
	}
	_ = cluster.group.SetAvailable(3, false)
	if err := store.Put([]byte("after-remove"), []byte("current-config")); err != nil {
		t.Fatal(err)
	}
	cluster.assertAll(t, "after-remove", "current-config", []uint64{1, 2, 4})
}

func TestLinearizableReadsRequireQuorum(t *testing.T) {
	cluster := newTestCluster(t, 3)
	if err := cluster.group.Elect(1); err != nil {
		t.Fatal(err)
	}
	store, _ := NewReplicatedStore(cluster.group, cluster.nodes[1].data)
	if err := store.Put([]byte("key"), []byte("value")); err != nil {
		t.Fatal(err)
	}
	_ = cluster.group.SetPartition(1, 2, true)
	_ = cluster.group.SetPartition(1, 3, true)
	if _, _, err := store.Get([]byte("key")); !errors.Is(err, ErrNoQuorum) {
		t.Fatalf("isolated read error = %v", err)
	}
}

type testCluster struct {
	root  string
	group *Group
	nodes map[uint64]*testMember
}

type testMember struct {
	node        *Node
	journal     *lsm.Store
	data        *lsm.Store
	journalPath string
	dataPath    string
}

func newTestCluster(t *testing.T, size int) *testCluster {
	t.Helper()
	cluster := &testCluster{root: t.TempDir(), nodes: make(map[uint64]*testMember)}
	members := make([]uint64, size)
	for index := range members {
		members[index] = uint64(index + 1)
	}
	var nodes []*Node
	for _, id := range members {
		member := cluster.newNode(t, id, members)
		nodes = append(nodes, member.node)
	}
	group, err := NewGroup(77, nodes, members)
	if err != nil {
		t.Fatal(err)
	}
	cluster.group = group
	return cluster
}

func (cluster *testCluster) newNode(t *testing.T, id uint64, members []uint64) *testMember {
	t.Helper()
	member := &testMember{
		journalPath: filepath.Join(cluster.root, fmt.Sprintf("node-%d-journal", id)),
		dataPath:    filepath.Join(cluster.root, fmt.Sprintf("node-%d-data", id)),
	}
	var err error
	member.journal, err = lsm.Open(member.journalPath)
	if err != nil {
		t.Fatal(err)
	}
	member.data, err = lsm.Open(member.dataPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = member.journal.Close(); _ = member.data.Close() })
	machine, err := NewKVStateMachine(member.data, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	member.node, err = OpenNode(77, id, member.journal, machine, members)
	if err != nil {
		t.Fatal(err)
	}
	cluster.nodes[id] = member
	return member
}

func (cluster *testCluster) assertAll(t *testing.T, key, expected string, ids []uint64) {
	t.Helper()
	for _, id := range ids {
		value, found, err := cluster.nodes[id].data.Get([]byte(key))
		if err != nil || !found || string(value) != expected {
			t.Fatalf("node %d get %q = (%q, %v, %v), want %q", id, key, value, found, err, expected)
		}
	}
}
