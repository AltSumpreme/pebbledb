package raft

import (
	"fmt"
	"math/rand"
	"testing"
)

func TestSeededPartitionCrashAndRecoverySimulation(t *testing.T) {
	cluster := newTestCluster(t, 3)
	random := rand.New(rand.NewSource(20260810))
	model := make(map[string]string)
	for epoch := 0; epoch < 20; epoch++ {
		healCluster(t, cluster)
		leaderID := electMostUpToDate(t, cluster)
		store, _ := NewReplicatedStore(cluster.group, cluster.nodes[leaderID].data)
		for operation := 0; operation < 12; operation++ {
			switch random.Intn(5) {
			case 0:
				peer := uint64(random.Intn(3) + 1)
				if peer != leaderID {
					_ = cluster.group.SetPartition(leaderID, peer, true)
				}
			case 1:
				peer := uint64(random.Intn(3) + 1)
				if peer != leaderID {
					_ = cluster.group.SetAvailable(peer, false)
				}
			default:
				key := fmt.Sprintf("committed/%02d/%02d", epoch, operation)
				value := fmt.Sprintf("value-%d", random.Int63())
				if err := store.Put([]byte(key), []byte(value)); err == nil {
					model[key] = value
				} else {
					// A timeout/no-quorum proposal has an indeterminate outcome. It
					// uses a unique key and is intentionally outside the acknowledged model.
					_ = store.Put([]byte(fmt.Sprintf("uncertain/%02d/%02d", epoch, operation)), []byte(value))
				}
			}
		}
		healCluster(t, cluster)
		leaderID = electMostUpToDate(t, cluster)
		store, _ = NewReplicatedStore(cluster.group, cluster.nodes[leaderID].data)
		if err := store.Put([]byte(fmt.Sprintf("barrier/%02d", epoch)), []byte("committed")); err != nil {
			t.Fatalf("epoch %d recovery barrier: %v", epoch, err)
		}
		if err := cluster.group.LinearizableRead(); err != nil {
			t.Fatalf("epoch %d read barrier: %v", epoch, err)
		}
		for key, expected := range model {
			cluster.assertAll(t, key, expected, []uint64{1, 2, 3})
		}
	}
}

func healCluster(t *testing.T, cluster *testCluster) {
	t.Helper()
	for id := uint64(1); id <= 3; id++ {
		if err := cluster.group.SetAvailable(id, true); err != nil {
			t.Fatal(err)
		}
		for other := id + 1; other <= 3; other++ {
			if err := cluster.group.SetPartition(id, other, false); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func electMostUpToDate(t *testing.T, cluster *testCluster) uint64 {
	t.Helper()
	bestID, bestIndex, bestTerm := uint64(0), uint64(0), uint64(0)
	for id, member := range cluster.nodes {
		member.node.mu.Lock()
		index, term := member.node.lastIndexLocked(), member.node.lastTermLocked()
		member.node.mu.Unlock()
		if bestID == 0 || term > bestTerm || (term == bestTerm && index > bestIndex) {
			bestID, bestIndex, bestTerm = id, index, term
		}
	}
	if err := cluster.group.Elect(bestID); err != nil {
		t.Fatalf("elect most up-to-date node %d: %v", bestID, err)
	}
	return bestID
}
