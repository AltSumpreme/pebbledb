// Package ranges implements persistent byte-key range descriptors and routing
// across local or remote KV replicas. Range boundaries are inclusive at Start
// and exclusive at End; an empty boundary represents infinity.
package ranges

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"sort"
	"sync"

	"pebbledb/codec"
	"pebbledb/storage/kv"
)

var (
	ErrNoRange         = errors.New("ranges: no range contains key")
	ErrStaleDescriptor = errors.New("ranges: stale range descriptor")
	ErrCrossRangeBatch = errors.New("ranges: atomic batch crosses range boundaries")
	ErrInvalidSplit    = errors.New("ranges: invalid split key")
)

const descriptorVersion byte = 1

var descriptorKeyPrefix = []byte{0x00, 'p', 'd', 'b', '-', 'r', 'n', 'g', 0x01}

// Descriptor is durable routing metadata for one contiguous key span.
type Descriptor struct {
	ID         uint64
	Start      []byte
	End        []byte
	ReplicaID  uint64
	Generation uint64
}

// Replica binds a stable placement ID to a storage endpoint.
type Replica struct {
	ID    uint64
	Store kv.BatchStore
}

// Router implements kv.BatchStore over a sorted, gap-free range map.
type Router struct {
	mu          sync.RWMutex
	metadata    kv.BatchStore
	replicas    map[uint64]kv.BatchStore
	descriptors []Descriptor
}

// Open loads routing metadata or bootstraps one full-keyspace range on initial.
// Every replica referenced by existing metadata must be supplied.
func Open(metadata kv.BatchStore, initial Replica, additional ...Replica) (*Router, error) {
	if metadata == nil {
		return nil, fmt.Errorf("ranges: metadata store cannot be nil")
	}
	replicas := make(map[uint64]kv.BatchStore, 1+len(additional))
	for _, replica := range append([]Replica{initial}, additional...) {
		if replica.ID == 0 || replica.Store == nil {
			return nil, fmt.Errorf("ranges: replica ID and store are required")
		}
		if _, exists := replicas[replica.ID]; exists {
			return nil, fmt.Errorf("ranges: duplicate replica ID %d", replica.ID)
		}
		replicas[replica.ID] = replica.Store
	}
	router := &Router{metadata: metadata, replicas: replicas}
	entries, err := metadata.Scan(descriptorKeyPrefix, codec.PrefixEnd(descriptorKeyPrefix))
	if err != nil {
		return nil, fmt.Errorf("ranges: load descriptors: %w", err)
	}
	for _, entry := range entries {
		descriptor, err := decodeDescriptor(entry.Value)
		if err != nil {
			return nil, fmt.Errorf("ranges: decode descriptor %x: %w", entry.Key, err)
		}
		if !bytes.Equal(entry.Key, descriptorKey(descriptor.ID)) {
			return nil, fmt.Errorf("ranges: descriptor key does not match ID %d", descriptor.ID)
		}
		router.descriptors = append(router.descriptors, descriptor)
	}
	if len(router.descriptors) == 0 {
		descriptor := Descriptor{ID: 1, ReplicaID: initial.ID, Generation: 1}
		encoded, err := encodeDescriptor(descriptor)
		if err != nil {
			return nil, err
		}
		if err := metadata.Apply([]kv.Mutation{{Key: descriptorKey(descriptor.ID), Value: encoded}}); err != nil {
			return nil, fmt.Errorf("ranges: bootstrap descriptor: %w", err)
		}
		router.descriptors = []Descriptor{descriptor}
	}
	if err := router.validateAndSort(); err != nil {
		return nil, err
	}
	return router, nil
}

// Descriptors returns a detached, key-ordered routing snapshot.
func (router *Router) Descriptors() []Descriptor {
	router.mu.RLock()
	defer router.mu.RUnlock()
	result := make([]Descriptor, len(router.descriptors))
	for index := range router.descriptors {
		result[index] = cloneDescriptor(router.descriptors[index])
	}
	return result
}

// Route returns the current descriptor for key.
func (router *Router) Route(key []byte) (Descriptor, error) {
	router.mu.RLock()
	defer router.mu.RUnlock()
	descriptor, err := router.routeLocked(key)
	if err != nil {
		return Descriptor{}, err
	}
	return cloneDescriptor(*descriptor), nil
}

func (router *Router) Get(key []byte) ([]byte, bool, error) {
	router.mu.RLock()
	defer router.mu.RUnlock()
	descriptor, err := router.routeLocked(key)
	if err != nil {
		return nil, false, err
	}
	return router.replicas[descriptor.ReplicaID].Get(key)
}

func (router *Router) Put(key, value []byte) error {
	return router.Apply([]kv.Mutation{{Key: key, Value: value}})
}

func (router *Router) Delete(key []byte) error {
	return router.Apply([]kv.Mutation{{Key: key, Delete: true}})
}

// Apply routes an atomic batch. A batch spanning ranges is rejected until the
// distributed transaction coordinator is introduced.
func (router *Router) Apply(mutations []kv.Mutation) error {
	if len(mutations) == 0 {
		return nil
	}
	router.mu.RLock()
	defer router.mu.RUnlock()
	descriptor, err := router.routeLocked(mutations[0].Key)
	if err != nil {
		return err
	}
	for _, mutation := range mutations[1:] {
		current, err := router.routeLocked(mutation.Key)
		if err != nil {
			return err
		}
		if current.ID != descriptor.ID {
			return ErrCrossRangeBatch
		}
	}
	return router.replicas[descriptor.ReplicaID].Apply(mutations)
}

// ApplyToRange processes a range-addressed request and rejects stale routing
// metadata. This is the request boundary used by later network transports.
func (router *Router) ApplyToRange(rangeID, generation uint64, mutations []kv.Mutation) error {
	router.mu.RLock()
	defer router.mu.RUnlock()
	descriptor := router.byIDLocked(rangeID)
	if descriptor == nil || descriptor.Generation != generation {
		return ErrStaleDescriptor
	}
	for _, mutation := range mutations {
		if !contains(*descriptor, mutation.Key) {
			return ErrStaleDescriptor
		}
	}
	return router.replicas[descriptor.ReplicaID].Apply(mutations)
}

// Scan merges the intersecting ranges in key order.
func (router *Router) Scan(start, end []byte) ([]kv.Entry, error) {
	if len(end) > 0 && bytes.Compare(start, end) > 0 {
		return nil, fmt.Errorf("ranges: range start must not sort after range end")
	}
	router.mu.RLock()
	defer router.mu.RUnlock()
	var result []kv.Entry
	for _, descriptor := range router.descriptors {
		if !intersects(descriptor, start, end) {
			continue
		}
		localStart := maxStart(descriptor.Start, start)
		localEnd := minEnd(descriptor.End, end)
		entries, err := router.replicas[descriptor.ReplicaID].Scan(localStart, localEnd)
		if err != nil {
			return nil, fmt.Errorf("ranges: scan range %d: %w", descriptor.ID, err)
		}
		result = append(result, entries...)
	}
	return result, nil
}

// Split copies the right-hand span to a replica, atomically publishes two new
// descriptors, then removes the now-unrouted source copies. Publishing after
// copying makes interruption safe; duplicate source data is never routed.
func (router *Router) Split(splitKey []byte, rightRangeID uint64, right Replica) (Descriptor, Descriptor, error) {
	router.mu.Lock()
	defer router.mu.Unlock()
	if len(splitKey) == 0 || rightRangeID == 0 || right.ID == 0 || right.Store == nil {
		return Descriptor{}, Descriptor{}, ErrInvalidSplit
	}
	left := router.routePointerLocked(splitKey)
	if left == nil || bytes.Equal(splitKey, left.Start) || (len(left.End) > 0 && bytes.Compare(splitKey, left.End) >= 0) {
		return Descriptor{}, Descriptor{}, ErrInvalidSplit
	}
	if router.byIDLocked(rightRangeID) != nil {
		return Descriptor{}, Descriptor{}, fmt.Errorf("ranges: range ID %d already exists", rightRangeID)
	}
	if existing, ok := router.replicas[right.ID]; ok {
		// Placement IDs are stable. If this replica was registered at Open, use
		// that endpoint instead of replacing it through a split request.
		right.Store = existing
	}
	if entries, err := right.Store.Scan(splitKey, left.End); err != nil {
		return Descriptor{}, Descriptor{}, err
	} else if len(entries) != 0 {
		return Descriptor{}, Descriptor{}, fmt.Errorf("ranges: split destination is not empty")
	}

	source := router.replicas[left.ReplicaID]
	entries, err := source.Scan(splitKey, left.End)
	if err != nil {
		return Descriptor{}, Descriptor{}, fmt.Errorf("ranges: scan split source: %w", err)
	}
	entries = userEntries(entries)
	if err := applyEntries(right.Store, entries, false); err != nil {
		return Descriptor{}, Descriptor{}, fmt.Errorf("ranges: copy split data: %w", err)
	}
	originalEnd := clone(left.End)
	updatedLeft := cloneDescriptor(*left)
	updatedLeft.End = clone(splitKey)
	updatedLeft.Generation++
	rightDescriptor := Descriptor{
		ID: rightRangeID, Start: clone(splitKey), End: originalEnd,
		ReplicaID: right.ID, Generation: 1,
	}
	leftEncoded, _ := encodeDescriptor(updatedLeft)
	rightEncoded, _ := encodeDescriptor(rightDescriptor)
	if err := router.metadata.Apply([]kv.Mutation{
		{Key: descriptorKey(updatedLeft.ID), Value: leftEncoded},
		{Key: descriptorKey(rightDescriptor.ID), Value: rightEncoded},
	}); err != nil {
		return Descriptor{}, Descriptor{}, fmt.Errorf("ranges: publish split: %w", err)
	}
	router.replicas[right.ID] = right.Store
	*left = updatedLeft
	router.descriptors = append(router.descriptors, rightDescriptor)
	sort.Slice(router.descriptors, func(i, j int) bool {
		return bytes.Compare(router.descriptors[i].Start, router.descriptors[j].Start) < 0
	})
	if err := applyEntries(source, entries, true); err != nil {
		return cloneDescriptor(updatedLeft), cloneDescriptor(rightDescriptor), fmt.Errorf("ranges: split published but source cleanup failed: %w", err)
	}
	return cloneDescriptor(updatedLeft), cloneDescriptor(rightDescriptor), nil
}

func (router *Router) validateAndSort() error {
	sort.Slice(router.descriptors, func(i, j int) bool {
		return bytes.Compare(router.descriptors[i].Start, router.descriptors[j].Start) < 0
	})
	ids := make(map[uint64]struct{}, len(router.descriptors))
	for index, descriptor := range router.descriptors {
		if descriptor.ID == 0 || descriptor.ReplicaID == 0 || descriptor.Generation == 0 {
			return fmt.Errorf("ranges: descriptor contains zero ID or generation")
		}
		if _, duplicate := ids[descriptor.ID]; duplicate {
			return fmt.Errorf("ranges: duplicate range ID %d", descriptor.ID)
		}
		ids[descriptor.ID] = struct{}{}
		if router.replicas[descriptor.ReplicaID] == nil {
			return fmt.Errorf("ranges: replica %d for range %d is unavailable", descriptor.ReplicaID, descriptor.ID)
		}
		if len(descriptor.End) > 0 && bytes.Compare(descriptor.Start, descriptor.End) >= 0 {
			return fmt.Errorf("ranges: range %d has invalid bounds", descriptor.ID)
		}
		if index == 0 && len(descriptor.Start) != 0 {
			return fmt.Errorf("ranges: keyspace has a leading gap")
		}
		if index > 0 && !bytes.Equal(router.descriptors[index-1].End, descriptor.Start) {
			return fmt.Errorf("ranges: keyspace has a gap or overlap")
		}
	}
	if len(router.descriptors[len(router.descriptors)-1].End) != 0 {
		return fmt.Errorf("ranges: keyspace has a trailing gap")
	}
	return nil
}

func (router *Router) routeLocked(key []byte) (*Descriptor, error) {
	descriptor := router.routePointerLocked(key)
	if descriptor == nil {
		return nil, ErrNoRange
	}
	return descriptor, nil
}

func (router *Router) routePointerLocked(key []byte) *Descriptor {
	index := sort.Search(len(router.descriptors), func(index int) bool {
		return len(router.descriptors[index].End) == 0 || bytes.Compare(key, router.descriptors[index].End) < 0
	})
	if index == len(router.descriptors) || !contains(router.descriptors[index], key) {
		return nil
	}
	return &router.descriptors[index]
}

func (router *Router) byIDLocked(id uint64) *Descriptor {
	for index := range router.descriptors {
		if router.descriptors[index].ID == id {
			return &router.descriptors[index]
		}
	}
	return nil
}

func contains(descriptor Descriptor, key []byte) bool {
	return (len(descriptor.Start) == 0 || bytes.Compare(key, descriptor.Start) >= 0) &&
		(len(descriptor.End) == 0 || bytes.Compare(key, descriptor.End) < 0)
}

func intersects(descriptor Descriptor, start, end []byte) bool {
	return (len(end) == 0 || len(descriptor.Start) == 0 || bytes.Compare(descriptor.Start, end) < 0) &&
		(len(descriptor.End) == 0 || len(start) == 0 || bytes.Compare(descriptor.End, start) > 0)
}

func maxStart(left, right []byte) []byte {
	if len(left) == 0 || (len(right) > 0 && bytes.Compare(right, left) > 0) {
		return clone(right)
	}
	return clone(left)
}

func minEnd(left, right []byte) []byte {
	if len(left) == 0 {
		return clone(right)
	}
	if len(right) == 0 || bytes.Compare(left, right) < 0 {
		return clone(left)
	}
	return clone(right)
}

func applyEntries(store kv.BatchStore, entries []kv.Entry, deleteEntries bool) error {
	const chunkSize = 1024
	for start := 0; start < len(entries); start += chunkSize {
		end := start + chunkSize
		if end > len(entries) {
			end = len(entries)
		}
		mutations := make([]kv.Mutation, 0, end-start)
		for _, entry := range entries[start:end] {
			mutations = append(mutations, kv.Mutation{Key: entry.Key, Value: entry.Value, Delete: deleteEntries})
		}
		if err := store.Apply(mutations); err != nil {
			return err
		}
	}
	return nil
}

// Metadata may share a physical LSM with the first range during single-node
// bootstrap. Descriptors are addressed directly through metadata and must not
// be moved as user-range contents.
func userEntries(entries []kv.Entry) []kv.Entry {
	result := entries[:0]
	for _, entry := range entries {
		if !bytes.HasPrefix(entry.Key, descriptorKeyPrefix) {
			result = append(result, entry)
		}
	}
	return result
}

func descriptorKey(id uint64) []byte {
	key := append([]byte(nil), descriptorKeyPrefix...)
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], id)
	return append(key, encoded[:]...)
}

func encodeDescriptor(descriptor Descriptor) ([]byte, error) {
	if len(descriptor.Start) > int(^uint32(0)) || len(descriptor.End) > int(^uint32(0)) {
		return nil, fmt.Errorf("ranges: descriptor boundary is too large")
	}
	encoded := make([]byte, 1+8+8+8+4+4, 1+8+8+8+4+4+len(descriptor.Start)+len(descriptor.End)+4)
	encoded[0] = descriptorVersion
	binary.BigEndian.PutUint64(encoded[1:9], descriptor.ID)
	binary.BigEndian.PutUint64(encoded[9:17], descriptor.ReplicaID)
	binary.BigEndian.PutUint64(encoded[17:25], descriptor.Generation)
	binary.BigEndian.PutUint32(encoded[25:29], uint32(len(descriptor.Start)))
	binary.BigEndian.PutUint32(encoded[29:33], uint32(len(descriptor.End)))
	encoded = append(encoded, descriptor.Start...)
	encoded = append(encoded, descriptor.End...)
	var checksum [4]byte
	binary.BigEndian.PutUint32(checksum[:], crc32.ChecksumIEEE(encoded))
	return append(encoded, checksum[:]...), nil
}

func decodeDescriptor(encoded []byte) (Descriptor, error) {
	if len(encoded) < 37 || encoded[0] != descriptorVersion {
		return Descriptor{}, fmt.Errorf("invalid descriptor header")
	}
	payload, checksum := encoded[:len(encoded)-4], encoded[len(encoded)-4:]
	if crc32.ChecksumIEEE(payload) != binary.BigEndian.Uint32(checksum) {
		return Descriptor{}, fmt.Errorf("descriptor checksum mismatch")
	}
	startLength := binary.BigEndian.Uint32(encoded[25:29])
	endLength := binary.BigEndian.Uint32(encoded[29:33])
	if uint64(33)+uint64(startLength)+uint64(endLength)+4 != uint64(len(encoded)) {
		return Descriptor{}, fmt.Errorf("descriptor length mismatch")
	}
	startEnd := 33 + int(startLength)
	return Descriptor{
		ID: binary.BigEndian.Uint64(encoded[1:9]), ReplicaID: binary.BigEndian.Uint64(encoded[9:17]),
		Generation: binary.BigEndian.Uint64(encoded[17:25]),
		Start:      clone(encoded[33:startEnd]), End: clone(encoded[startEnd : len(encoded)-4]),
	}, nil
}

func cloneDescriptor(descriptor Descriptor) Descriptor {
	descriptor.Start, descriptor.End = clone(descriptor.Start), clone(descriptor.End)
	return descriptor
}

func clone(value []byte) []byte { return append([]byte(nil), value...) }
