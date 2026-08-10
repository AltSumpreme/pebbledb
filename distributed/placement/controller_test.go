package placement

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestPlanRestoresReplicationBeforeOtherActions(t *testing.T) {
	controller := NewController(Policy{ReplicationFactor: 3})
	state := ClusterState{
		Nodes: []Node{
			{ID: 1, Live: true, CapacityBytes: 100, UsedBytes: 80},
			{ID: 2, Live: true, CapacityBytes: 100, UsedBytes: 40},
			{ID: 3, Live: true, CapacityBytes: 100, UsedBytes: 10},
		},
		Ranges: []Range{{ID: 7, Bytes: 1 << 30, Leaseholder: 1, Replicas: []uint64{1}}},
	}
	actions, err := controller.Plan(state)
	if err != nil || len(actions) != 1 || actions[0].Kind != AddReplica || actions[0].ToNode != 3 {
		t.Fatalf("actions=%+v err=%v", actions, err)
	}
}

func TestPlanSplitsOversizedAndHotRanges(t *testing.T) {
	controller := NewController(Policy{ReplicationFactor: 1, MaxRangeBytes: 100, HotQueriesPerSec: 1000, EWMAAlpha: 1})
	state := ClusterState{
		Nodes: []Node{{ID: 1, Live: true, CapacityBytes: 1000, UsedBytes: 100}},
		Ranges: []Range{
			{ID: 4, Start: []byte("a"), End: []byte("m"), Bytes: 101, Leaseholder: 1, Replicas: []uint64{1}, SuggestedSplit: []byte("g")},
			{ID: 9, Start: []byte("m"), Bytes: 50, QueriesPerSec: 1200, Leaseholder: 1, Replicas: []uint64{1}, SuggestedSplit: []byte("t")},
		},
	}
	actions, err := controller.Plan(state)
	if err != nil || len(actions) != 2 || actions[0].Kind != SplitRange || actions[1].Kind != SplitRange || actions[0].NewRangeID != 10 || actions[1].NewRangeID != 11 {
		t.Fatalf("actions=%+v err=%v", actions, err)
	}
}

func TestPlanTransfersLeaseAndRebalancesReplica(t *testing.T) {
	policy := Policy{ReplicationFactor: 2, HotQueriesPerSec: 1000, OverloadUtilization: 0.8, RebalanceMinDelta: 0.2, EWMAAlpha: 1}
	controller := NewController(policy)
	state := ClusterState{
		Nodes: []Node{
			{ID: 1, Live: true, CapacityBytes: 100, UsedBytes: 95, QueriesPerSec: 900, LeaseCount: 100},
			{ID: 2, Live: true, CapacityBytes: 100, UsedBytes: 30, LeaseCount: 1},
			{ID: 3, Live: true, CapacityBytes: 100, UsedBytes: 5},
		},
		Ranges: []Range{
			{ID: 1, End: []byte("m"), Bytes: 60, Leaseholder: 1, Replicas: []uint64{1, 2}},
			{ID: 2, Start: []byte("m"), Bytes: 60, Leaseholder: 2, Replicas: []uint64{1, 2}},
		},
	}
	actions, err := controller.Plan(state)
	if err != nil || len(actions) != 2 {
		t.Fatalf("actions=%+v err=%v", actions, err)
	}
	if actions[0].Kind != TransferLease || actions[0].FromNode != 1 || actions[0].ToNode != 2 {
		t.Fatalf("lease action=%+v", actions[0])
	}
	if actions[1].Kind != MoveReplica || actions[1].FromNode != 1 || actions[1].ToNode != 3 {
		t.Fatalf("rebalance action=%+v", actions[1])
	}
}

func TestPlanReplacesDeadReplicaThenRemovesIt(t *testing.T) {
	controller := NewController(Policy{ReplicationFactor: 2})
	nodes := []Node{
		{ID: 1, Live: true, CapacityBytes: 100, UsedBytes: 20},
		{ID: 2, Live: false, CapacityBytes: 100, UsedBytes: 20},
		{ID: 3, Live: true, CapacityBytes: 100, UsedBytes: 10},
	}
	actions, err := controller.Plan(ClusterState{Nodes: nodes, Ranges: []Range{{ID: 1, Leaseholder: 1, Replicas: []uint64{1, 2}}}})
	if err != nil || len(actions) != 1 || actions[0].Kind != AddReplica || actions[0].ToNode != 3 {
		t.Fatalf("replacement=%+v err=%v", actions, err)
	}
	actions, err = controller.Plan(ClusterState{Nodes: nodes, Ranges: []Range{{ID: 1, Leaseholder: 1, Replicas: []uint64{1, 2, 3}}}})
	if err != nil || len(actions) != 1 || actions[0].Kind != RemoveReplica || actions[0].FromNode != 2 {
		t.Fatalf("removal=%+v err=%v", actions, err)
	}
}

func TestPlanMergesAdjacentColdRanges(t *testing.T) {
	controller := NewController(Policy{ReplicationFactor: 1, MaxRangeBytes: 1000, MergeBelowBytes: 100, HotQueriesPerSec: 1000, EWMAAlpha: 1})
	state := ClusterState{
		Nodes: []Node{{ID: 1, Live: true, CapacityBytes: 1000, UsedBytes: 100}},
		Ranges: []Range{
			{ID: 5, End: []byte("m"), Bytes: 30, QueriesPerSec: 10, Leaseholder: 1, Replicas: []uint64{1}},
			{ID: 6, Start: []byte("m"), Bytes: 40, QueriesPerSec: 10, Leaseholder: 1, Replicas: []uint64{1}},
		},
	}
	actions, err := controller.Plan(state)
	if err != nil || len(actions) != 1 || actions[0].Kind != MergeRanges || actions[0].RangeID != 5 || actions[0].OtherRangeID != 6 {
		t.Fatalf("actions=%+v err=%v", actions, err)
	}
}

func TestReconcileExecutesActionsAndRunCancels(t *testing.T) {
	controller := NewController(Policy{ReplicationFactor: 2, ReconcileInterval: time.Millisecond, MaxActionsPerCycle: 1})
	source := staticSource{state: ClusterState{
		Nodes:  []Node{{ID: 1, Live: true, CapacityBytes: 100, UsedBytes: 20}, {ID: 2, Live: true, CapacityBytes: 100, UsedBytes: 10}},
		Ranges: []Range{{ID: 1, Leaseholder: 1, Replicas: []uint64{1}}},
	}}
	orchestrator := &recordingOrchestrator{}
	actions, err := controller.Reconcile(context.Background(), source, orchestrator)
	if err != nil || len(actions) != 1 || len(orchestrator.actions) != 1 {
		t.Fatalf("reconcile actions=%+v recorded=%+v err=%v", actions, orchestrator.actions, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := controller.Run(ctx, source, orchestrator); !errors.Is(err, context.Canceled) {
		t.Fatalf("run cancellation error=%v", err)
	}
}

type staticSource struct{ state ClusterState }

func (source staticSource) Snapshot(context.Context) (ClusterState, error) { return source.state, nil }

type recordingOrchestrator struct {
	mu      sync.Mutex
	actions []Action
}

func (orchestrator *recordingOrchestrator) Execute(_ context.Context, action Action) error {
	orchestrator.mu.Lock()
	orchestrator.actions = append(orchestrator.actions, action)
	orchestrator.mu.Unlock()
	return nil
}
