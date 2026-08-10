package raft

import (
	"errors"
	"fmt"
	"sort"
	"sync"
)

// Group is a deterministic in-process Raft transport and coordinator. It is
// deliberately synchronous, which makes quorum outcomes explicit and makes the
// same code suitable for deterministic partition/recovery tests.
type Group struct {
	mu       sync.Mutex
	id       uint64
	nodes    map[uint64]*Node
	members  []uint64
	leaderID uint64
	blocked  map[link]bool
}

type link struct{ from, to uint64 }

func NewGroup(id uint64, nodes []*Node, suppliedMembers ...[]uint64) (*Group, error) {
	if id == 0 || len(nodes) == 0 {
		return nil, fmt.Errorf("raft: group ID and nodes are required")
	}
	group := &Group{id: id, nodes: make(map[uint64]*Node, len(nodes)), blocked: make(map[link]bool)}
	for _, node := range nodes {
		if node == nil || node.groupID != id {
			return nil, fmt.Errorf("raft: node belongs to another group")
		}
		if _, exists := group.nodes[node.id]; exists {
			return nil, fmt.Errorf("raft: duplicate node ID %d", node.id)
		}
		group.nodes[node.id] = node
	}
	if len(suppliedMembers) > 1 {
		return nil, fmt.Errorf("raft: at most one membership may be supplied")
	}
	if len(suppliedMembers) == 1 {
		group.members = normalizedMembers(suppliedMembers[0])
	} else {
		group.members = normalizedMembers(nodes[0].state.Members)
	}
	if len(group.members) == 0 {
		return nil, fmt.Errorf("raft: membership cannot be empty")
	}
	for _, id := range group.members {
		if group.nodes[id] == nil {
			return nil, fmt.Errorf("raft: member %d has no node", id)
		}
	}
	return group, nil
}

func (group *Group) LeaderID() uint64 {
	group.mu.Lock()
	defer group.mu.Unlock()
	return group.leaderID
}

func (group *Group) Members() []uint64 {
	group.mu.Lock()
	defer group.mu.Unlock()
	return append([]uint64(nil), group.members...)
}

// Elect runs a full RequestVote round for candidateID.
func (group *Group) Elect(candidateID uint64) error {
	group.mu.Lock()
	defer group.mu.Unlock()
	candidate := group.nodes[candidateID]
	if candidate == nil || !containsMember(group.members, candidateID) {
		return ErrUnknownNode
	}
	candidate.mu.Lock()
	if !candidate.available {
		candidate.mu.Unlock()
		return ErrUnavailable
	}
	candidate.state.Term++
	term := candidate.state.Term
	candidate.state.VotedFor, candidate.role = candidateID, Candidate
	lastIndex, lastTerm := candidate.lastIndexLocked(), candidate.lastTermLocked()
	if err := candidate.persistStateLocked(nil); err != nil {
		candidate.mu.Unlock()
		return err
	}
	candidate.mu.Unlock()

	votes := 1
	for _, memberID := range group.members {
		if memberID == candidateID || !group.connected(candidateID, memberID) {
			continue
		}
		response, err := group.nodes[memberID].requestVote(term, candidateID, lastIndex, lastTerm)
		if err != nil {
			continue
		}
		if response.term > term {
			candidate.mu.Lock()
			candidate.state.Term, candidate.state.VotedFor, candidate.role = response.term, 0, Follower
			_ = candidate.persistStateLocked(nil)
			candidate.mu.Unlock()
			group.leaderID = 0
			return ErrNoQuorum
		}
		if response.granted {
			votes++
		}
	}
	if votes < quorum(len(group.members)) {
		candidate.mu.Lock()
		candidate.role = Follower
		candidate.mu.Unlock()
		group.leaderID = 0
		return ErrNoQuorum
	}
	for _, node := range group.nodes {
		node.mu.Lock()
		if node.id == candidateID {
			node.role = Leader
		} else if node.state.Term <= term {
			node.role = Follower
		}
		node.mu.Unlock()
	}
	group.leaderID = candidateID
	// Establish authority and repair reachable follower logs.
	for _, memberID := range group.members {
		if memberID != candidateID && group.connected(candidateID, memberID) {
			_, _ = group.syncFollowerLocked(candidate, group.nodes[memberID])
		}
	}
	return nil
}

// Propose replicates and commits one application command.
func (group *Group) Propose(command []byte) (uint64, error) {
	group.mu.Lock()
	defer group.mu.Unlock()
	return group.proposeLocked(Entry{Type: CommandEntry, Command: append([]byte(nil), command...)})
}

func (group *Group) proposeLocked(entry Entry) (uint64, error) {
	leader := group.nodes[group.leaderID]
	if leader == nil {
		return 0, ErrNotLeader
	}
	leader.mu.Lock()
	if !leader.available || leader.role != Leader {
		leader.mu.Unlock()
		return 0, ErrNotLeader
	}
	entry.Index, entry.Term = leader.lastIndexLocked()+1, leader.state.Term
	if err := leader.appendLocalLocked(entry); err != nil {
		leader.mu.Unlock()
		return 0, err
	}
	leader.mu.Unlock()

	acknowledged := 1
	for _, memberID := range group.members {
		if memberID == leader.id || !group.connected(leader.id, memberID) {
			continue
		}
		matched, err := group.syncFollowerLocked(leader, group.nodes[memberID])
		if err == nil && matched >= entry.Index {
			acknowledged++
		}
	}
	if acknowledged < quorum(len(group.members)) {
		return 0, ErrNoQuorum
	}
	leader.mu.Lock()
	if err := leader.commitLocked(entry.Index); err != nil {
		leader.mu.Unlock()
		return 0, err
	}
	leader.mu.Unlock()
	for _, memberID := range group.members {
		if memberID != leader.id && group.connected(leader.id, memberID) {
			_, _ = group.syncFollowerLocked(leader, group.nodes[memberID])
		}
	}
	return entry.Index, nil
}

// LinearizableRead confirms the leader can contact a current-term quorum and
// catches reachable followers up through the committed index.
func (group *Group) LinearizableRead() error {
	group.mu.Lock()
	defer group.mu.Unlock()
	leader := group.nodes[group.leaderID]
	if leader == nil {
		return ErrNotLeader
	}
	leader.mu.Lock()
	available, role := leader.available, leader.role
	leader.mu.Unlock()
	if !available || role != Leader {
		return ErrNotLeader
	}
	acknowledged := 1
	for _, memberID := range group.members {
		if memberID == leader.id || !group.connected(leader.id, memberID) {
			continue
		}
		if _, err := group.syncFollowerLocked(leader, group.nodes[memberID]); err == nil {
			acknowledged++
		}
	}
	if acknowledged < quorum(len(group.members)) {
		return ErrNoQuorum
	}
	return nil
}

// Snapshot compacts the leader log at its fully applied index. Lagging
// followers receive this snapshot during their next synchronization.
func (group *Group) Snapshot() (uint64, error) {
	group.mu.Lock()
	defer group.mu.Unlock()
	leader := group.nodes[group.leaderID]
	if leader == nil {
		return 0, ErrNotLeader
	}
	leader.mu.Lock()
	snapshot, err := leader.createSnapshotLocked()
	leader.mu.Unlock()
	return snapshot.Index, err
}

// AddMember catches up a registered non-voter, then commits a configuration
// entry using the old quorum before it becomes a voter.
func (group *Group) AddMember(node *Node) error {
	group.mu.Lock()
	defer group.mu.Unlock()
	if node == nil || node.groupID != group.id || containsMember(group.members, node.id) {
		return ErrInvalidConfig
	}
	if existing := group.nodes[node.id]; existing != nil && existing != node {
		return ErrInvalidConfig
	}
	group.nodes[node.id] = node
	leader := group.nodes[group.leaderID]
	if leader == nil {
		return ErrNotLeader
	}
	if !group.connected(leader.id, node.id) {
		return ErrUnavailable
	}
	if _, err := group.syncFollowerLocked(leader, node); err != nil {
		return err
	}
	updated := normalizedMembers(append(group.members, node.id))
	if _, err := group.proposeLocked(Entry{Type: ConfigurationEntry, Members: updated}); err != nil {
		return err
	}
	group.members = updated
	if _, err := group.syncFollowerLocked(leader, node); err != nil {
		return err
	}
	return nil
}

// RemoveMember commits removal under the old membership. Removing the current
// leader clears leadership so a remaining member must be elected.
func (group *Group) RemoveMember(nodeID uint64) error {
	group.mu.Lock()
	defer group.mu.Unlock()
	if !containsMember(group.members, nodeID) || len(group.members) == 1 {
		return ErrInvalidConfig
	}
	updated := make([]uint64, 0, len(group.members)-1)
	for _, current := range group.members {
		if current != nodeID {
			updated = append(updated, current)
		}
	}
	if _, err := group.proposeLocked(Entry{Type: ConfigurationEntry, Members: updated}); err != nil {
		return err
	}
	group.members = updated
	if group.leaderID == nodeID {
		group.leaderID = 0
	}
	return nil
}

func (group *Group) SetAvailable(nodeID uint64, available bool) error {
	group.mu.Lock()
	defer group.mu.Unlock()
	node := group.nodes[nodeID]
	if node == nil {
		return ErrUnknownNode
	}
	node.mu.Lock()
	node.available = available
	if !available && group.leaderID == nodeID {
		node.role = Follower
		group.leaderID = 0
	}
	node.mu.Unlock()
	return nil
}

// SetPartition toggles a bidirectional transport partition between two nodes.
func (group *Group) SetPartition(left, right uint64, blocked bool) error {
	group.mu.Lock()
	defer group.mu.Unlock()
	if group.nodes[left] == nil || group.nodes[right] == nil {
		return ErrUnknownNode
	}
	group.blocked[link{left, right}] = blocked
	group.blocked[link{right, left}] = blocked
	return nil
}

func (group *Group) syncFollowerLocked(leader, follower *Node) (uint64, error) {
	leader.mu.Lock()
	if !leader.available || leader.role != Leader {
		leader.mu.Unlock()
		return 0, ErrNotLeader
	}
	term, commit, snapshotIndex := leader.state.Term, leader.state.CommitIndex, leader.state.SnapshotIndex
	members := append([]uint64(nil), leader.state.Members...)
	var snapshot *persistedSnapshot
	if leader.snapshot != nil {
		copyValue := *leader.snapshot
		copyValue.Data = append([]byte(nil), copyValue.Data...)
		snapshot = &copyValue
	}
	leader.mu.Unlock()

	follower.mu.Lock()
	followerLast := follower.lastIndexLocked()
	follower.mu.Unlock()
	if followerLast < snapshotIndex {
		if snapshot == nil {
			return 0, fmt.Errorf("raft: leader snapshot is missing")
		}
		if err := follower.installSnapshot(term, *snapshot, members); err != nil {
			return 0, err
		}
		followerLast = snapshotIndex
	}

	previous := followerLast
	for {
		leader.mu.Lock()
		leaderTerm := leader.termAtLocked(previous)
		leader.mu.Unlock()
		follower.mu.Lock()
		followerTerm := follower.termAtLocked(previous)
		follower.mu.Unlock()
		if previous == 0 || (leaderTerm != 0 && leaderTerm == followerTerm) {
			break
		}
		previous--
		if previous < snapshotIndex {
			if snapshot == nil {
				return 0, fmt.Errorf("raft: cannot repair follower before compacted log")
			}
			if err := follower.installSnapshot(term, *snapshot, members); err != nil {
				return 0, err
			}
			previous = snapshotIndex
			break
		}
	}
	leader.mu.Lock()
	previousTerm := leader.termAtLocked(previous)
	entries := leader.entriesAfterLocked(previous)
	leader.mu.Unlock()
	response, err := follower.appendEntries(term, leader.id, previous, previousTerm, entries, commit)
	if err != nil {
		return 0, err
	}
	if response.term > term {
		leader.mu.Lock()
		leader.state.Term, leader.state.VotedFor, leader.role = response.term, 0, Follower
		_ = leader.persistStateLocked(nil)
		leader.mu.Unlock()
		group.leaderID = 0
		return 0, ErrNotLeader
	}
	if !response.success {
		return 0, errors.New("raft: follower rejected matching log")
	}
	return response.matchIndex, nil
}

func (group *Group) connected(from, to uint64) bool {
	if group.blocked[link{from, to}] {
		return false
	}
	node := group.nodes[to]
	if node == nil {
		return false
	}
	node.mu.Lock()
	available := node.available
	node.mu.Unlock()
	return available
}

func quorum(members int) int { return members/2 + 1 }

func sortedNodeIDs(nodes map[uint64]*Node) []uint64 {
	result := make([]uint64, 0, len(nodes))
	for id := range nodes {
		result = append(result, id)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}
