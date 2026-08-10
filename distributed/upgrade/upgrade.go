// Package upgrade coordinates monotonic, leader-owned rolling binary upgrades.
// Nodes roll to binaries that understand both the active and target versions
// before the cluster activates new behavior.
package upgrade

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"sort"
	"sync"

	"pebbledb/storage/kv"
)

type Version uint32

const (
	BaselineVersion Version = 1
	CurrentVersion  Version = 2
	MinimumVersion  Version = BaselineVersion
)

var (
	ErrIncompatible      = errors.New("upgrade: binary is incompatible with cluster version")
	ErrUpgradeInProgress = errors.New("upgrade: another upgrade is already in progress")
	ErrInvalidTransition = errors.New("upgrade: invalid version transition")
	ErrNotReady          = errors.New("upgrade: not every node has acknowledged the target")
	ErrUnknownNode       = errors.New("upgrade: unknown node")
)

var stateKey = []byte{0x00, 'p', 'd', 'b', '-', 'u', 'p', 'g', 'r', 'a', 'd', 'e', 0x01}

type Binary struct {
	NodeID string
	Min    Version
	Max    Version
}

type NodeStatus struct {
	NodeID       string
	Min          Version
	Max          Version
	Acknowledged Version
}

type Status struct {
	Active     Version
	Target     Version
	Generation uint64
	Nodes      []NodeStatus
}

type stateRecord struct {
	Active     Version      `json:"active"`
	Target     Version      `json:"target,omitempty"`
	Generation uint64       `json:"generation"`
	Nodes      []NodeStatus `json:"nodes"`
}

// Coordinator must be mutated only by the current cluster-version leaseholder.
// The durable generation detects stale cached views; consensus/lease ownership
// supplies the single-writer guarantee, as it does for range metadata changes.
type Coordinator struct {
	mu    sync.Mutex
	store kv.Store
	state stateRecord
}

// Open loads the cluster version or initializes a new cluster at bootstrap.
func Open(store kv.Store, bootstrap Version) (*Coordinator, error) {
	if store == nil || bootstrap < BaselineVersion {
		return nil, fmt.Errorf("upgrade: store and valid bootstrap version are required")
	}
	coordinator := &Coordinator{store: store}
	encoded, found, err := store.Get(stateKey)
	if err != nil {
		return nil, err
	}
	if !found {
		coordinator.state = stateRecord{Active: bootstrap, Generation: 1}
		if err := coordinator.persist(coordinator.state); err != nil {
			return nil, fmt.Errorf("upgrade: bootstrap state: %w", err)
		}
		return coordinator, nil
	}
	if err := decodeRecord(encoded, &coordinator.state); err != nil {
		return nil, fmt.Errorf("upgrade: load state: %w", err)
	}
	if err := validateState(coordinator.state); err != nil {
		return nil, err
	}
	return coordinator, nil
}

// Register records a node's supported version interval. During a pending
// activation, binaries that cannot understand the target are refused.
func (coordinator *Coordinator) Register(binary Binary) error {
	return coordinator.update(func(state *stateRecord) error {
		if err := validateBinary(binary); err != nil {
			return err
		}
		if !supports(binary, state.Active) || (state.Target != 0 && !supports(binary, state.Target)) {
			return fmt.Errorf("%w: node %s supports %d-%d, cluster active=%d target=%d", ErrIncompatible, binary.NodeID, binary.Min, binary.Max, state.Active, state.Target)
		}
		index := nodeIndex(state.Nodes, binary.NodeID)
		acknowledged := state.Active
		if index >= 0 && state.Nodes[index].Acknowledged >= state.Active && state.Nodes[index].Acknowledged <= binary.Max {
			acknowledged = state.Nodes[index].Acknowledged
		}
		node := NodeStatus{NodeID: binary.NodeID, Min: binary.Min, Max: binary.Max, Acknowledged: acknowledged}
		if index < 0 {
			state.Nodes = append(state.Nodes, node)
		} else {
			state.Nodes[index] = node
		}
		sortNodes(state.Nodes)
		return nil
	})
}

// Remove decommissions a node. At least one registered node must remain.
func (coordinator *Coordinator) Remove(nodeID string) error {
	return coordinator.update(func(state *stateRecord) error {
		index := nodeIndex(state.Nodes, nodeID)
		if index < 0 {
			return ErrUnknownNode
		}
		if len(state.Nodes) == 1 {
			return fmt.Errorf("upgrade: cannot remove the final registered node")
		}
		state.Nodes = append(state.Nodes[:index], state.Nodes[index+1:]...)
		return nil
	})
}

// Begin starts the next consecutive version activation after every registered
// binary has been rolled to one that understands it.
func (coordinator *Coordinator) Begin(target Version) error {
	return coordinator.update(func(state *stateRecord) error {
		if state.Target != 0 {
			return ErrUpgradeInProgress
		}
		if target != state.Active+1 {
			return ErrInvalidTransition
		}
		if len(state.Nodes) == 0 {
			return fmt.Errorf("%w: no registered nodes", ErrNotReady)
		}
		for index := range state.Nodes {
			node := &state.Nodes[index]
			if target < node.Min || target > node.Max {
				return fmt.Errorf("%w: node %s supports %d-%d", ErrIncompatible, node.NodeID, node.Min, node.Max)
			}
			node.Acknowledged = state.Active
		}
		state.Target = target
		return nil
	})
}

// Acknowledge marks a rolled node ready. Features remain gated by Active until
// Finalize, so acknowledgment itself never changes data formats or behavior.
func (coordinator *Coordinator) Acknowledge(nodeID string, target Version) error {
	return coordinator.update(func(state *stateRecord) error {
		if state.Target == 0 || state.Target != target {
			return ErrInvalidTransition
		}
		index := nodeIndex(state.Nodes, nodeID)
		if index < 0 {
			return ErrUnknownNode
		}
		state.Nodes[index].Acknowledged = target
		return nil
	})
}

// Finalize atomically activates the target after every node acknowledges it.
// Active versions never move backwards.
func (coordinator *Coordinator) Finalize(target Version) error {
	return coordinator.update(func(state *stateRecord) error {
		if state.Target == 0 || state.Target != target {
			return ErrInvalidTransition
		}
		for _, node := range state.Nodes {
			if node.Acknowledged != target {
				return fmt.Errorf("%w: node %s acknowledged %d", ErrNotReady, node.NodeID, node.Acknowledged)
			}
		}
		state.Active, state.Target = target, 0
		for index := range state.Nodes {
			state.Nodes[index].Acknowledged = target
		}
		return nil
	})
}

// Abort cancels a pending activation. Active behavior has not changed yet, so
// it is safe even after readiness acknowledgments.
func (coordinator *Coordinator) Abort(target Version) error {
	return coordinator.update(func(state *stateRecord) error {
		if state.Target == 0 || state.Target != target {
			return ErrInvalidTransition
		}
		state.Target = 0
		for index := range state.Nodes {
			state.Nodes[index].Acknowledged = state.Active
		}
		return nil
	})
}

func (coordinator *Coordinator) Status() Status {
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	return statusOf(coordinator.state)
}

func (coordinator *Coordinator) update(change func(*stateRecord) error) error {
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	latest, err := coordinator.load()
	if err != nil {
		return err
	}
	next := cloneState(latest)
	if err := change(&next); err != nil {
		return err
	}
	next.Generation++
	if err := validateState(next); err != nil {
		return err
	}
	if err := coordinator.persist(next); err != nil {
		return err
	}
	coordinator.state = next
	return nil
}

func (coordinator *Coordinator) load() (stateRecord, error) {
	encoded, found, err := coordinator.store.Get(stateKey)
	if err != nil {
		return stateRecord{}, err
	}
	if !found {
		return stateRecord{}, fmt.Errorf("upgrade: durable state disappeared")
	}
	var state stateRecord
	if err := decodeRecord(encoded, &state); err != nil {
		return stateRecord{}, err
	}
	if state.Generation < coordinator.state.Generation {
		return stateRecord{}, fmt.Errorf("upgrade: durable generation moved backwards")
	}
	return state, validateState(state)
}

func (coordinator *Coordinator) persist(state stateRecord) error {
	encoded, err := encodeRecord(state)
	if err != nil {
		return err
	}
	return coordinator.store.Put(stateKey, encoded)
}

func validateBinary(binary Binary) error {
	if binary.NodeID == "" || binary.Min < BaselineVersion || binary.Max < binary.Min {
		return fmt.Errorf("upgrade: invalid binary support interval")
	}
	return nil
}

func validateState(state stateRecord) error {
	if state.Active < BaselineVersion || state.Generation == 0 || (state.Target != 0 && state.Target != state.Active+1) {
		return fmt.Errorf("upgrade: invalid durable version state")
	}
	for index, node := range state.Nodes {
		if err := validateBinary(Binary{NodeID: node.NodeID, Min: node.Min, Max: node.Max}); err != nil {
			return err
		}
		if index > 0 && state.Nodes[index-1].NodeID >= node.NodeID {
			return fmt.Errorf("upgrade: nodes are not uniquely sorted")
		}
		validAcknowledgment := node.Acknowledged == state.Active || (state.Target != 0 && node.Acknowledged == state.Target)
		if !validAcknowledgment || node.Acknowledged < node.Min || node.Acknowledged > node.Max {
			return fmt.Errorf("upgrade: invalid node acknowledgment")
		}
	}
	return nil
}

func supports(binary Binary, version Version) bool {
	return version >= binary.Min && version <= binary.Max
}

func nodeIndex(nodes []NodeStatus, nodeID string) int {
	for index := range nodes {
		if nodes[index].NodeID == nodeID {
			return index
		}
	}
	return -1
}

func sortNodes(nodes []NodeStatus) {
	sort.Slice(nodes, func(left, right int) bool { return nodes[left].NodeID < nodes[right].NodeID })
}

func statusOf(state stateRecord) Status {
	return Status{Active: state.Active, Target: state.Target, Generation: state.Generation, Nodes: append([]NodeStatus(nil), state.Nodes...)}
}

func cloneState(state stateRecord) stateRecord {
	state.Nodes = append([]NodeStatus(nil), state.Nodes...)
	return state
}

func encodeRecord(state stateRecord) ([]byte, error) {
	payload, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	encoded := append([]byte{1}, payload...)
	var checksum [4]byte
	binary.BigEndian.PutUint32(checksum[:], crc32.ChecksumIEEE(encoded))
	return append(encoded, checksum[:]...), nil
}

func decodeRecord(encoded []byte, destination *stateRecord) error {
	if len(encoded) < 6 || encoded[0] != 1 {
		return fmt.Errorf("invalid record header")
	}
	payload, checksum := encoded[:len(encoded)-4], encoded[len(encoded)-4:]
	if crc32.ChecksumIEEE(payload) != binary.BigEndian.Uint32(checksum) {
		return fmt.Errorf("record checksum mismatch")
	}
	if err := json.Unmarshal(payload[1:], destination); err != nil {
		return err
	}
	reencoded, err := encodeRecord(*destination)
	if err != nil || !bytes.Equal(reencoded, encoded) {
		return fmt.Errorf("record is not canonical")
	}
	return nil
}
