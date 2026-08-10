// Package placement turns cluster capacity and load observations into
// deterministic range-management actions.
package placement

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

type ActionKind string

const (
	AddReplica    ActionKind = "add-replica"
	RemoveReplica ActionKind = "remove-replica"
	MoveReplica   ActionKind = "move-replica"
	TransferLease ActionKind = "transfer-lease"
	SplitRange    ActionKind = "split-range"
	MergeRanges   ActionKind = "merge-ranges"
)

type Node struct {
	ID            uint64
	Live          bool
	CapacityBytes uint64
	UsedBytes     uint64
	QueriesPerSec float64
	LeaseCount    int
	Locality      string
}

type Range struct {
	ID             uint64
	Start          []byte
	End            []byte
	Bytes          uint64
	QueriesPerSec  float64
	Leaseholder    uint64
	Replicas       []uint64
	SuggestedSplit []byte
}

type ClusterState struct {
	Nodes  []Node
	Ranges []Range
}

type Action struct {
	Kind         ActionKind
	RangeID      uint64
	OtherRangeID uint64
	NewRangeID   uint64
	FromNode     uint64
	ToNode       uint64
	SplitKey     []byte
	Reason       string
}

type Policy struct {
	ReplicationFactor   int
	MaxRangeBytes       uint64
	MergeBelowBytes     uint64
	HotQueriesPerSec    float64
	OverloadUtilization float64
	RebalanceMinDelta   float64
	EWMAAlpha           float64
	ReconcileInterval   time.Duration
	MaxActionsPerCycle  int
}

func (policy Policy) normalized() Policy {
	if policy.ReplicationFactor <= 0 {
		policy.ReplicationFactor = 3
	}
	if policy.MaxRangeBytes == 0 {
		policy.MaxRangeBytes = 512 << 20
	}
	if policy.MergeBelowBytes == 0 {
		policy.MergeBelowBytes = policy.MaxRangeBytes / 4
	}
	if policy.HotQueriesPerSec <= 0 {
		policy.HotQueriesPerSec = 10_000
	}
	if policy.OverloadUtilization <= 0 || policy.OverloadUtilization > 1 {
		policy.OverloadUtilization = 0.85
	}
	if policy.RebalanceMinDelta <= 0 || policy.RebalanceMinDelta > 1 {
		policy.RebalanceMinDelta = 0.20
	}
	if policy.EWMAAlpha <= 0 || policy.EWMAAlpha > 1 {
		policy.EWMAAlpha = 0.35
	}
	if policy.ReconcileInterval <= 0 {
		policy.ReconcileInterval = 10 * time.Second
	}
	if policy.MaxActionsPerCycle <= 0 {
		policy.MaxActionsPerCycle = 64
	}
	return policy
}

type Controller struct {
	mu     sync.Mutex
	policy Policy
	hot    map[uint64]float64
}

func NewController(policy Policy) *Controller {
	return &Controller{policy: policy.normalized(), hot: make(map[uint64]float64)}
}

// Observe updates a range's smoothed request rate independently of a planning
// cycle, allowing heartbeat ingestion and placement to run at different rates.
func (controller *Controller) Observe(rangeID uint64, queriesPerSecond float64) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	controller.observeLocked(rangeID, queriesPerSecond)
}

// Plan returns at most one action per range, ordered by safety first (replica
// health), then split/merge policy, lease locality, and capacity balancing.
func (controller *Controller) Plan(state ClusterState) ([]Action, error) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	if err := validateState(state); err != nil {
		return nil, err
	}
	nodes := make(map[uint64]Node, len(state.Nodes))
	for _, node := range state.Nodes {
		nodes[node.ID] = node
	}
	ranges := append([]Range(nil), state.Ranges...)
	sort.Slice(ranges, func(i, j int) bool { return bytes.Compare(ranges[i].Start, ranges[j].Start) < 0 })
	nextRangeID := uint64(1)
	for _, current := range ranges {
		if current.ID >= nextRangeID {
			nextRangeID = current.ID + 1
		}
		controller.observeLocked(current.ID, current.QueriesPerSec)
	}
	claimed := make(map[uint64]bool, len(ranges))
	actions := make([]Action, 0)
	appendAction := func(action Action) {
		if len(actions) < controller.policy.MaxActionsPerCycle {
			actions = append(actions, action)
			claimed[action.RangeID] = true
			if action.OtherRangeID != 0 {
				claimed[action.OtherRangeID] = true
			}
		}
	}

	for _, current := range ranges {
		if len(actions) >= controller.policy.MaxActionsPerCycle {
			break
		}
		liveReplicas := 0
		var deadReplica uint64
		for _, replica := range current.Replicas {
			if nodes[replica].Live {
				liveReplicas++
			} else if deadReplica == 0 {
				deadReplica = replica
			}
		}
		if liveReplicas < controller.policy.ReplicationFactor {
			if target := bestTarget(nodes, current.Replicas); target != 0 {
				appendAction(Action{Kind: AddReplica, RangeID: current.ID, ToNode: target, Reason: "restore replication quorum"})
			}
			continue
		}
		if deadReplica != 0 && len(current.Replicas) > controller.policy.ReplicationFactor {
			appendAction(Action{Kind: RemoveReplica, RangeID: current.ID, FromNode: deadReplica, Reason: "remove unavailable replica after replacement"})
			continue
		}
		if len(current.Replicas) > controller.policy.ReplicationFactor {
			if source := worstReplica(nodes, current.Replicas); source != 0 {
				appendAction(Action{Kind: RemoveReplica, RangeID: current.ID, FromNode: source, Reason: "remove excess replica"})
				continue
			}
		}
		hot := controller.hot[current.ID]
		if (current.Bytes > controller.policy.MaxRangeBytes || hot >= controller.policy.HotQueriesPerSec) && validSplit(current) {
			appendAction(Action{
				Kind: SplitRange, RangeID: current.ID, NewRangeID: nextRangeID,
				SplitKey: clone(current.SuggestedSplit), Reason: splitReason(current.Bytes, hot, controller.policy),
			})
			nextRangeID++
			continue
		}
		leaseNode := nodes[current.Leaseholder]
		if target := bestLeaseTarget(nodes, current, controller.policy, hot); target != 0 && target != current.Leaseholder {
			appendAction(Action{
				Kind: TransferLease, RangeID: current.ID, FromNode: current.Leaseholder, ToNode: target,
				Reason: fmt.Sprintf("move lease from utilization %.2f", utilization(leaseNode)),
			})
			continue
		}
		if source, target := rebalancePair(nodes, current.Replicas, controller.policy); source != 0 {
			appendAction(Action{Kind: MoveReplica, RangeID: current.ID, FromNode: source, ToNode: target, Reason: "rebalance capacity"})
		}
	}

	for index := 0; index+1 < len(ranges) && len(actions) < controller.policy.MaxActionsPerCycle; index++ {
		left, right := ranges[index], ranges[index+1]
		if claimed[left.ID] || claimed[right.ID] || !bytes.Equal(left.End, right.Start) {
			continue
		}
		if left.Bytes+right.Bytes > controller.policy.MergeBelowBytes || controller.hot[left.ID] >= controller.policy.HotQueriesPerSec/4 || controller.hot[right.ID] >= controller.policy.HotQueriesPerSec/4 {
			continue
		}
		if !sameMembers(left.Replicas, right.Replicas) {
			continue
		}
		appendAction(Action{Kind: MergeRanges, RangeID: left.ID, OtherRangeID: right.ID, Reason: "merge adjacent cold ranges"})
		index++
	}
	return actions, nil
}

type StateSource interface {
	Snapshot(context.Context) (ClusterState, error)
}

type Orchestrator interface {
	Execute(context.Context, Action) error
}

func (controller *Controller) Reconcile(ctx context.Context, source StateSource, orchestrator Orchestrator) ([]Action, error) {
	if source == nil || orchestrator == nil {
		return nil, fmt.Errorf("placement: source and orchestrator are required")
	}
	state, err := source.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	actions, err := controller.Plan(state)
	if err != nil {
		return nil, err
	}
	for _, action := range actions {
		if err := orchestrator.Execute(ctx, action); err != nil {
			return actions, fmt.Errorf("placement: execute %s for range %d: %w", action.Kind, action.RangeID, err)
		}
	}
	return actions, nil
}

// Run reconciles immediately and then on every policy interval until canceled.
func (controller *Controller) Run(ctx context.Context, source StateSource, orchestrator Orchestrator) error {
	if _, err := controller.Reconcile(ctx, source, orchestrator); err != nil {
		return err
	}
	ticker := time.NewTicker(controller.policy.ReconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if _, err := controller.Reconcile(ctx, source, orchestrator); err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (controller *Controller) observeLocked(rangeID uint64, qps float64) {
	if qps < 0 {
		qps = 0
	}
	previous, exists := controller.hot[rangeID]
	if !exists {
		controller.hot[rangeID] = qps
		return
	}
	alpha := controller.policy.EWMAAlpha
	controller.hot[rangeID] = alpha*qps + (1-alpha)*previous
}

func validateState(state ClusterState) error {
	nodes := make(map[uint64]struct{}, len(state.Nodes))
	for _, node := range state.Nodes {
		if node.ID == 0 || node.CapacityBytes == 0 || node.UsedBytes > node.CapacityBytes {
			return fmt.Errorf("placement: invalid node %d capacity", node.ID)
		}
		if _, duplicate := nodes[node.ID]; duplicate {
			return fmt.Errorf("placement: duplicate node %d", node.ID)
		}
		nodes[node.ID] = struct{}{}
	}
	ranges := make(map[uint64]struct{}, len(state.Ranges))
	for _, current := range state.Ranges {
		if current.ID == 0 || len(current.Replicas) == 0 {
			return fmt.Errorf("placement: invalid range %d", current.ID)
		}
		if _, duplicate := ranges[current.ID]; duplicate {
			return fmt.Errorf("placement: duplicate range %d", current.ID)
		}
		ranges[current.ID] = struct{}{}
		seen := make(map[uint64]struct{}, len(current.Replicas))
		for _, replica := range current.Replicas {
			if _, exists := nodes[replica]; !exists {
				return fmt.Errorf("placement: range %d references unknown node %d", current.ID, replica)
			}
			if _, duplicate := seen[replica]; duplicate {
				return fmt.Errorf("placement: range %d has duplicate replica %d", current.ID, replica)
			}
			seen[replica] = struct{}{}
		}
		if _, exists := seen[current.Leaseholder]; !exists {
			return fmt.Errorf("placement: range %d leaseholder is not a replica", current.ID)
		}
	}
	return nil
}

func bestTarget(nodes map[uint64]Node, existing []uint64) uint64 {
	excluded := memberSet(existing)
	var best uint64
	bestScore := 2.0
	for id, node := range nodes {
		if !node.Live || excluded[id] {
			continue
		}
		score := utilization(node)
		if best == 0 || score < bestScore || (score == bestScore && id < best) {
			best, bestScore = id, score
		}
	}
	return best
}

func worstReplica(nodes map[uint64]Node, replicas []uint64) uint64 {
	var worst uint64
	worstScore := -1.0
	for _, id := range replicas {
		node := nodes[id]
		score := utilization(node)
		if !node.Live {
			score = 2
		}
		if score > worstScore || (score == worstScore && id > worst) {
			worst, worstScore = id, score
		}
	}
	return worst
}

func bestLeaseTarget(nodes map[uint64]Node, current Range, policy Policy, hot float64) uint64 {
	lease := nodes[current.Leaseholder]
	if lease.Live && utilization(lease) < policy.OverloadUtilization && hot < policy.HotQueriesPerSec {
		return 0
	}
	best, bestScore := uint64(0), 3.0
	for _, id := range current.Replicas {
		node := nodes[id]
		if !node.Live {
			continue
		}
		score := utilization(node) + float64(node.LeaseCount)/1000 + node.QueriesPerSec/(policy.HotQueriesPerSec*10)
		if best == 0 || score < bestScore || (score == bestScore && id < best) {
			best, bestScore = id, score
		}
	}
	return best
}

func rebalancePair(nodes map[uint64]Node, replicas []uint64, policy Policy) (uint64, uint64) {
	source := worstReplica(nodes, replicas)
	target := bestTarget(nodes, replicas)
	if source == 0 || target == 0 || utilization(nodes[source])-utilization(nodes[target]) < policy.RebalanceMinDelta {
		return 0, 0
	}
	return source, target
}

func validSplit(current Range) bool {
	return len(current.SuggestedSplit) > 0 &&
		(len(current.Start) == 0 || bytes.Compare(current.SuggestedSplit, current.Start) > 0) &&
		(len(current.End) == 0 || bytes.Compare(current.SuggestedSplit, current.End) < 0)
}

func splitReason(bytesValue uint64, hot float64, policy Policy) string {
	if bytesValue > policy.MaxRangeBytes {
		return "split oversized range"
	}
	return fmt.Sprintf("split hot range at %.0f qps", hot)
}

func utilization(node Node) float64 {
	if node.CapacityBytes == 0 {
		return 1
	}
	return float64(node.UsedBytes) / float64(node.CapacityBytes)
}

func sameMembers(left, right []uint64) bool {
	leftCopy, rightCopy := append([]uint64(nil), left...), append([]uint64(nil), right...)
	sort.Slice(leftCopy, func(i, j int) bool { return leftCopy[i] < leftCopy[j] })
	sort.Slice(rightCopy, func(i, j int) bool { return rightCopy[i] < rightCopy[j] })
	if len(leftCopy) != len(rightCopy) {
		return false
	}
	for index := range leftCopy {
		if leftCopy[index] != rightCopy[index] {
			return false
		}
	}
	return true
}

func memberSet(ids []uint64) map[uint64]bool {
	result := make(map[uint64]bool, len(ids))
	for _, id := range ids {
		result[id] = true
	}
	return result
}

func clone(value []byte) []byte { return append([]byte(nil), value...) }
