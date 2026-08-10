// Package raft implements PebbleDB's synchronous Raft consensus core. The
// transport is in-process today, but elections, log matching, quorum commit,
// persistent hard state, snapshots, and membership entries follow the same
// state-machine boundaries used by a future network transport.
package raft

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"sort"
	"sync"

	"pebbledb/codec"
	"pebbledb/storage/kv"
)

var (
	ErrNotLeader     = errors.New("raft: node is not the leader")
	ErrNoQuorum      = errors.New("raft: quorum is unavailable")
	ErrUnavailable   = errors.New("raft: node is unavailable")
	ErrUnknownNode   = errors.New("raft: unknown node")
	ErrInvalidConfig = errors.New("raft: invalid membership change")
)

type Role uint8

const (
	Follower Role = iota + 1
	Candidate
	Leader
)

type EntryType uint8

const (
	CommandEntry EntryType = iota + 1
	ConfigurationEntry
)

type Entry struct {
	Index   uint64    `json:"index"`
	Term    uint64    `json:"term"`
	Type    EntryType `json:"type"`
	Command []byte    `json:"command,omitempty"`
	Members []uint64  `json:"members,omitempty"`
}

// StateMachine is deterministic application state driven by committed entries.
type StateMachine interface {
	Apply(command []byte) error
	Snapshot() ([]byte, error)
	Restore(snapshot []byte) error
}

type hardState struct {
	Term          uint64   `json:"term"`
	VotedFor      uint64   `json:"voted_for"`
	CommitIndex   uint64   `json:"commit_index"`
	LastApplied   uint64   `json:"last_applied"`
	SnapshotIndex uint64   `json:"snapshot_index"`
	SnapshotTerm  uint64   `json:"snapshot_term"`
	Members       []uint64 `json:"members"`
}

type persistedSnapshot struct {
	Index uint64 `json:"index"`
	Term  uint64 `json:"term"`
	Data  []byte `json:"data"`
}

// Node is one durable member of a consensus group.
type Node struct {
	mu        sync.Mutex
	groupID   uint64
	id        uint64
	journal   kv.BatchStore
	machine   StateMachine
	state     hardState
	log       []Entry
	role      Role
	available bool
	snapshot  *persistedSnapshot
}

var raftKeyPrefix = []byte{0x00, 'p', 'd', 'b', '-', 'r', 'a', 'f', 't', 0x01}

const (
	hardStateKind byte = 1
	logKind       byte = 2
	snapshotKind  byte = 3
	formatVersion byte = 1
)

// OpenNode loads a node and replays every committed entry not represented by
// its snapshot. initialMembers is used only for a brand-new journal.
func OpenNode(groupID, nodeID uint64, journal kv.BatchStore, machine StateMachine, initialMembers []uint64) (*Node, error) {
	if groupID == 0 || nodeID == 0 || journal == nil || machine == nil {
		return nil, fmt.Errorf("raft: group, node, journal, and state machine are required")
	}
	node := &Node{groupID: groupID, id: nodeID, journal: journal, machine: machine, role: Follower, available: true}
	stateValue, found, err := journal.Get(node.hardStateKey())
	if err != nil {
		return nil, err
	}
	if found {
		if err := decodeRecord(stateValue, &node.state); err != nil {
			return nil, fmt.Errorf("raft: decode hard state: %w", err)
		}
	} else {
		node.state.Members = normalizedMembers(initialMembers)
		if !containsMember(node.state.Members, nodeID) {
			return nil, fmt.Errorf("raft: initial membership excludes node %d", nodeID)
		}
		if err := node.persistStateLocked(nil); err != nil {
			return nil, err
		}
	}
	snapshotValue, found, err := journal.Get(node.snapshotKey())
	if err != nil {
		return nil, err
	}
	if found {
		var snapshot persistedSnapshot
		if err := decodeRecord(snapshotValue, &snapshot); err != nil {
			return nil, fmt.Errorf("raft: decode snapshot: %w", err)
		}
		if snapshot.Index != node.state.SnapshotIndex || snapshot.Term != node.state.SnapshotTerm {
			return nil, fmt.Errorf("raft: snapshot does not match hard state")
		}
		if err := machine.Restore(snapshot.Data); err != nil {
			return nil, fmt.Errorf("raft: restore snapshot: %w", err)
		}
		node.snapshot = &snapshot
	}
	entries, err := journal.Scan(node.logPrefix(), codec.PrefixEnd(node.logPrefix()))
	if err != nil {
		return nil, fmt.Errorf("raft: load log: %w", err)
	}
	for _, stored := range entries {
		var entry Entry
		if err := decodeRecord(stored.Value, &entry); err != nil {
			return nil, fmt.Errorf("raft: decode log entry: %w", err)
		}
		if !bytes.Equal(stored.Key, node.logKey(entry.Index)) {
			return nil, fmt.Errorf("raft: log key/index mismatch")
		}
		node.log = append(node.log, entry)
	}
	if err := node.validateLogLocked(); err != nil {
		return nil, err
	}
	// Restoring a snapshot resets machine state. Replay all later committed
	// commands, which are idempotent at the KV state machine boundary.
	node.state.LastApplied = node.state.SnapshotIndex
	if err := node.applyCommittedLocked(); err != nil {
		return nil, err
	}
	return node, nil
}

func (node *Node) ID() uint64      { return node.id }
func (node *Node) GroupID() uint64 { return node.groupID }

func (node *Node) Status() Status {
	node.mu.Lock()
	defer node.mu.Unlock()
	return Status{
		NodeID: node.id, Term: node.state.Term, Role: node.role,
		CommitIndex: node.state.CommitIndex, LastApplied: node.state.LastApplied,
		LastLogIndex: node.lastIndexLocked(), Available: node.available,
		Members: append([]uint64(nil), node.state.Members...),
	}
}

type Status struct {
	NodeID       uint64
	Term         uint64
	Role         Role
	CommitIndex  uint64
	LastApplied  uint64
	LastLogIndex uint64
	Available    bool
	Members      []uint64
}

type voteResponse struct {
	term    uint64
	granted bool
}

func (node *Node) requestVote(term, candidateID, lastIndex, lastTerm uint64) (voteResponse, error) {
	node.mu.Lock()
	defer node.mu.Unlock()
	if !node.available {
		return voteResponse{}, ErrUnavailable
	}
	if term < node.state.Term {
		return voteResponse{term: node.state.Term}, nil
	}
	if term > node.state.Term {
		node.state.Term, node.state.VotedFor, node.role = term, 0, Follower
	}
	localLastIndex, localLastTerm := node.lastIndexLocked(), node.lastTermLocked()
	upToDate := lastTerm > localLastTerm || (lastTerm == localLastTerm && lastIndex >= localLastIndex)
	granted := upToDate && (node.state.VotedFor == 0 || node.state.VotedFor == candidateID)
	if granted {
		node.state.VotedFor = candidateID
	}
	if err := node.persistStateLocked(nil); err != nil {
		return voteResponse{}, err
	}
	return voteResponse{term: node.state.Term, granted: granted}, nil
}

type appendResponse struct {
	term       uint64
	success    bool
	matchIndex uint64
}

func (node *Node) appendEntries(term, leaderID, previousIndex, previousTerm uint64, entries []Entry, leaderCommit uint64) (appendResponse, error) {
	node.mu.Lock()
	defer node.mu.Unlock()
	if !node.available {
		return appendResponse{}, ErrUnavailable
	}
	if term < node.state.Term {
		return appendResponse{term: node.state.Term}, nil
	}
	if term > node.state.Term {
		node.state.Term, node.state.VotedFor = term, 0
	}
	node.role = Follower
	if previousIndex > 0 && node.termAtLocked(previousIndex) != previousTerm {
		if err := node.persistStateLocked(nil); err != nil {
			return appendResponse{}, err
		}
		return appendResponse{term: node.state.Term}, nil
	}

	var mutations []kv.Mutation
	for position, incoming := range entries {
		if incoming.Index <= node.lastIndexLocked() {
			if node.termAtLocked(incoming.Index) == incoming.Term {
				continue
			}
			mutations = append(mutations, node.truncateMutationsLocked(incoming.Index)...)
			node.truncateMemoryLocked(incoming.Index)
		}
		for _, remaining := range entries[position:] {
			encoded, err := encodeRecord(remaining)
			if err != nil {
				return appendResponse{}, err
			}
			mutations = append(mutations, kv.Mutation{Key: node.logKey(remaining.Index), Value: encoded})
			node.log = append(node.log, cloneEntry(remaining))
		}
		break
	}
	if leaderCommit > node.state.CommitIndex {
		node.state.CommitIndex = min(leaderCommit, node.lastIndexLocked())
	}
	if err := node.persistStateLocked(mutations); err != nil {
		return appendResponse{}, err
	}
	if err := node.applyCommittedLocked(); err != nil {
		return appendResponse{}, err
	}
	return appendResponse{term: node.state.Term, success: true, matchIndex: node.lastIndexLocked()}, nil
}

func (node *Node) appendLocalLocked(entry Entry) error {
	encoded, err := encodeRecord(entry)
	if err != nil {
		return err
	}
	node.log = append(node.log, cloneEntry(entry))
	if err := node.persistStateLocked([]kv.Mutation{{Key: node.logKey(entry.Index), Value: encoded}}); err != nil {
		node.log = node.log[:len(node.log)-1]
		return err
	}
	return nil
}

func (node *Node) commitLocked(index uint64) error {
	if index > node.lastIndexLocked() {
		return fmt.Errorf("raft: cannot commit missing index %d", index)
	}
	if index > node.state.CommitIndex {
		node.state.CommitIndex = index
		if err := node.persistStateLocked(nil); err != nil {
			return err
		}
	}
	return node.applyCommittedLocked()
}

func (node *Node) applyCommittedLocked() error {
	for node.state.LastApplied < node.state.CommitIndex {
		index := node.state.LastApplied + 1
		if index <= node.state.SnapshotIndex {
			node.state.LastApplied = index
			continue
		}
		entry, found := node.entryAtLocked(index)
		if !found {
			return fmt.Errorf("raft: committed entry %d is missing", index)
		}
		switch entry.Type {
		case CommandEntry:
			if err := node.machine.Apply(entry.Command); err != nil {
				return fmt.Errorf("raft: apply entry %d: %w", index, err)
			}
		case ConfigurationEntry:
			node.state.Members = normalizedMembers(entry.Members)
		default:
			return fmt.Errorf("raft: entry %d has unknown type %d", index, entry.Type)
		}
		node.state.LastApplied = index
		if err := node.persistStateLocked(nil); err != nil {
			return err
		}
	}
	return nil
}

func (node *Node) createSnapshotLocked() (persistedSnapshot, error) {
	if node.state.LastApplied == 0 {
		return persistedSnapshot{}, fmt.Errorf("raft: nothing has been applied")
	}
	data, err := node.machine.Snapshot()
	if err != nil {
		return persistedSnapshot{}, err
	}
	index := node.state.LastApplied
	term := node.termAtLocked(index)
	snapshot := persistedSnapshot{Index: index, Term: term, Data: data}
	encoded, err := encodeRecord(snapshot)
	if err != nil {
		return persistedSnapshot{}, err
	}
	mutations := []kv.Mutation{{Key: node.snapshotKey(), Value: encoded}}
	for _, entry := range node.log {
		if entry.Index <= index {
			mutations = append(mutations, kv.Mutation{Key: node.logKey(entry.Index), Delete: true})
		}
	}
	node.state.SnapshotIndex, node.state.SnapshotTerm = index, term
	if err := node.persistStateLocked(mutations); err != nil {
		return persistedSnapshot{}, err
	}
	remaining := node.log[:0]
	for _, entry := range node.log {
		if entry.Index > index {
			remaining = append(remaining, entry)
		}
	}
	node.log = remaining
	node.snapshot = &snapshot
	return snapshot, nil
}

func (node *Node) installSnapshot(term uint64, snapshot persistedSnapshot, members []uint64) error {
	node.mu.Lock()
	defer node.mu.Unlock()
	if !node.available {
		return ErrUnavailable
	}
	if term < node.state.Term {
		return ErrNotLeader
	}
	if err := node.machine.Restore(snapshot.Data); err != nil {
		return err
	}
	encoded, err := encodeRecord(snapshot)
	if err != nil {
		return err
	}
	mutations := []kv.Mutation{{Key: node.snapshotKey(), Value: encoded}}
	for _, entry := range node.log {
		if entry.Index <= snapshot.Index {
			mutations = append(mutations, kv.Mutation{Key: node.logKey(entry.Index), Delete: true})
		}
	}
	node.state.Term, node.state.VotedFor, node.role = term, 0, Follower
	node.state.SnapshotIndex, node.state.SnapshotTerm = snapshot.Index, snapshot.Term
	node.state.CommitIndex, node.state.LastApplied = snapshot.Index, snapshot.Index
	node.state.Members = append([]uint64(nil), members...)
	if err := node.persistStateLocked(mutations); err != nil {
		return err
	}
	remaining := node.log[:0]
	for _, entry := range node.log {
		if entry.Index > snapshot.Index {
			remaining = append(remaining, entry)
		}
	}
	node.log, node.snapshot = remaining, &snapshot
	return nil
}

func (node *Node) persistStateLocked(extra []kv.Mutation) error {
	encoded, err := encodeRecord(node.state)
	if err != nil {
		return err
	}
	mutations := append([]kv.Mutation(nil), extra...)
	mutations = append(mutations, kv.Mutation{Key: node.hardStateKey(), Value: encoded})
	return node.journal.Apply(mutations)
}

func (node *Node) validateLogLocked() error {
	expected := node.state.SnapshotIndex + 1
	for _, entry := range node.log {
		if entry.Index != expected || entry.Term == 0 {
			return fmt.Errorf("raft: log is not contiguous at index %d", expected)
		}
		expected++
	}
	if node.state.CommitIndex > node.lastIndexLocked() || node.state.LastApplied > node.state.CommitIndex {
		return fmt.Errorf("raft: hard state indexes are inconsistent")
	}
	return nil
}

func (node *Node) lastIndexLocked() uint64 {
	if len(node.log) == 0 {
		return node.state.SnapshotIndex
	}
	return node.log[len(node.log)-1].Index
}

func (node *Node) lastTermLocked() uint64 { return node.termAtLocked(node.lastIndexLocked()) }

func (node *Node) termAtLocked(index uint64) uint64 {
	if index == node.state.SnapshotIndex {
		return node.state.SnapshotTerm
	}
	if index < node.state.SnapshotIndex {
		return 0
	}
	offset := index - node.state.SnapshotIndex - 1
	if offset >= uint64(len(node.log)) {
		return 0
	}
	return node.log[offset].Term
}

func (node *Node) entryAtLocked(index uint64) (Entry, bool) {
	if index <= node.state.SnapshotIndex {
		return Entry{}, false
	}
	offset := index - node.state.SnapshotIndex - 1
	if offset >= uint64(len(node.log)) {
		return Entry{}, false
	}
	return node.log[offset], true
}

func (node *Node) entriesAfterLocked(index uint64) []Entry {
	if index < node.state.SnapshotIndex {
		return nil
	}
	offset := index - node.state.SnapshotIndex
	if offset >= uint64(len(node.log)) {
		return nil
	}
	result := make([]Entry, len(node.log[offset:]))
	for i := range result {
		result[i] = cloneEntry(node.log[int(offset)+i])
	}
	return result
}

func (node *Node) truncateMutationsLocked(from uint64) []kv.Mutation {
	var mutations []kv.Mutation
	for _, entry := range node.log {
		if entry.Index >= from {
			mutations = append(mutations, kv.Mutation{Key: node.logKey(entry.Index), Delete: true})
		}
	}
	return mutations
}

func (node *Node) truncateMemoryLocked(from uint64) {
	for index, entry := range node.log {
		if entry.Index >= from {
			node.log = node.log[:index]
			return
		}
	}
}

func (node *Node) key(kind byte, suffix ...byte) []byte {
	key := append([]byte(nil), raftKeyPrefix...)
	var identity [16]byte
	binary.BigEndian.PutUint64(identity[:8], node.groupID)
	binary.BigEndian.PutUint64(identity[8:], node.id)
	key = append(key, identity[:]...)
	key = append(key, kind)
	return append(key, suffix...)
}

func (node *Node) hardStateKey() []byte { return node.key(hardStateKind) }
func (node *Node) snapshotKey() []byte  { return node.key(snapshotKind) }
func (node *Node) logPrefix() []byte    { return node.key(logKind) }
func (node *Node) logKey(index uint64) []byte {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], index)
	return node.key(logKind, encoded[:]...)
}

func encodeRecord(value any) ([]byte, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	encoded := append([]byte{formatVersion}, payload...)
	var checksum [4]byte
	binary.BigEndian.PutUint32(checksum[:], crc32.ChecksumIEEE(encoded))
	return append(encoded, checksum[:]...), nil
}

func decodeRecord(encoded []byte, destination any) error {
	if len(encoded) < 6 || encoded[0] != formatVersion {
		return fmt.Errorf("invalid record header")
	}
	payload, checksum := encoded[:len(encoded)-4], encoded[len(encoded)-4:]
	if crc32.ChecksumIEEE(payload) != binary.BigEndian.Uint32(checksum) {
		return fmt.Errorf("record checksum mismatch")
	}
	if err := json.Unmarshal(payload[1:], destination); err != nil {
		return fmt.Errorf("decode record: %w", err)
	}
	return nil
}

func cloneEntry(entry Entry) Entry {
	entry.Command = append([]byte(nil), entry.Command...)
	entry.Members = append([]uint64(nil), entry.Members...)
	return entry
}

func normalizedMembers(members []uint64) []uint64 {
	seen := make(map[uint64]struct{}, len(members))
	result := make([]uint64, 0, len(members))
	for _, member := range members {
		if member == 0 {
			continue
		}
		if _, exists := seen[member]; !exists {
			seen[member] = struct{}{}
			result = append(result, member)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

func containsMember(members []uint64, id uint64) bool {
	index := sort.Search(len(members), func(index int) bool { return members[index] >= id })
	return index < len(members) && members[index] == id
}

func min(left, right uint64) uint64 {
	if left < right {
		return left
	}
	return right
}
