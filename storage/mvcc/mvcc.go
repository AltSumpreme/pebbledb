// Package mvcc provides timestamped snapshots and optimistic serializable
// transactions over PebbleDB's atomic ordered storage.
package mvcc

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"pebbledb/codec"
	"pebbledb/storage/kv"
)

var (
	ErrConflict = errors.New("mvcc: serializable transaction conflict")
	ErrClosed   = errors.New("mvcc: transaction is closed")
)

var versionKeyPrefix = []byte{0x00, 'p', 'd', 'b', '-', 'm', 'v', 'c', 'c', 0x01}

// PhysicalKeySpan returns the reserved storage span containing MVCC versions.
// Replication and range layers use it without interpreting version keys.
func PhysicalKeySpan() ([]byte, []byte) {
	return clone(versionKeyPrefix), codec.PrefixEnd(versionKeyPrefix)
}

// PhysicalSpan maps a logical inclusive/exclusive KV span to the physical span
// containing all of its timestamped versions.
func PhysicalSpan(logicalStart, logicalEnd []byte) ([]byte, []byte) {
	return versionBounds(logicalStart, logicalEnd)
}

type pendingWrite struct {
	value  []byte
	delete bool
}

type span struct {
	start []byte
	end   []byte
}

// Manager owns the timestamp oracle and serializable commit validation for one
// physical store. Applications should share one Manager per open database.
type Manager struct {
	mu    sync.Mutex
	store kv.BatchStore
	clock uint64
}

// NewManager recovers the timestamp oracle from durable MVCC versions.
func NewManager(store kv.BatchStore) (*Manager, error) {
	if store == nil {
		return nil, fmt.Errorf("mvcc: batch store cannot be nil")
	}
	manager := &Manager{store: store, clock: uint64(time.Now().UnixNano())}
	entries, err := store.Scan(versionKeyPrefix, codec.PrefixEnd(versionKeyPrefix))
	if err != nil {
		return nil, fmt.Errorf("mvcc: recover timestamp oracle: %w", err)
	}
	for _, entry := range entries {
		_, timestamp, err := decodeVersionKey(entry.Key)
		if err != nil {
			return nil, fmt.Errorf("mvcc: recover timestamp oracle: %w", err)
		}
		if timestamp > manager.clock {
			manager.clock = timestamp
		}
	}
	return manager, nil
}

// Begin creates a transaction whose reads observe a stable snapshot.
func (manager *Manager) Begin() *Transaction {
	manager.mu.Lock()
	manager.clock++
	readTimestamp := manager.clock
	manager.mu.Unlock()
	return &Transaction{
		manager:       manager,
		readTimestamp: readTimestamp,
		writes:        make(map[string]pendingWrite),
		readKeys:      make(map[string]struct{}),
	}
}

// Store returns an autocommit KV view. It is useful for administrative catalog
// access; SQL execution uses an explicit Transaction view for statement-level
// atomicity.
func (manager *Manager) Store() kv.Store { return &autocommitStore{manager: manager} }

type autocommitStore struct{ manager *Manager }

func (store *autocommitStore) Put(key, value []byte) error {
	txn := store.manager.Begin()
	if err := txn.Put(key, value); err != nil {
		return err
	}
	_, err := txn.Commit()
	return err
}

func (store *autocommitStore) Delete(key []byte) error {
	txn := store.manager.Begin()
	if err := txn.Delete(key); err != nil {
		return err
	}
	_, err := txn.Commit()
	return err
}

func (store *autocommitStore) Get(key []byte) ([]byte, bool, error) {
	txn := store.manager.Begin()
	defer txn.Rollback()
	return txn.Get(key)
}

func (store *autocommitStore) Scan(start, end []byte) ([]kv.Entry, error) {
	txn := store.manager.Begin()
	defer txn.Rollback()
	return txn.Scan(start, end)
}

// Transaction is a snapshot plus buffered write intents. It implements
// kv.Store so catalog, row, and index code all participate in one commit.
type Transaction struct {
	mu            sync.Mutex
	manager       *Manager
	readTimestamp uint64
	writes        map[string]pendingWrite
	readKeys      map[string]struct{}
	readSpans     []span
	closed        bool
}

func (transaction *Transaction) ReadTimestamp() uint64 { return transaction.readTimestamp }

func (transaction *Transaction) Get(key []byte) ([]byte, bool, error) {
	if len(key) == 0 {
		return nil, false, fmt.Errorf("mvcc: key cannot be empty")
	}
	transaction.mu.Lock()
	defer transaction.mu.Unlock()
	if transaction.closed {
		return nil, false, ErrClosed
	}
	transaction.readKeys[string(key)] = struct{}{}
	if write, exists := transaction.writes[string(key)]; exists {
		if write.delete {
			return nil, false, nil
		}
		return clone(write.value), true, nil
	}
	return transaction.manager.getAt(key, transaction.readTimestamp)
}

func (transaction *Transaction) Scan(start, end []byte) ([]kv.Entry, error) {
	if len(end) > 0 && bytes.Compare(start, end) > 0 {
		return nil, fmt.Errorf("mvcc: range start must not sort after range end")
	}
	transaction.mu.Lock()
	defer transaction.mu.Unlock()
	if transaction.closed {
		return nil, ErrClosed
	}
	transaction.readSpans = append(transaction.readSpans, span{start: clone(start), end: clone(end)})
	entries, err := transaction.manager.scanAt(start, end, transaction.readTimestamp)
	if err != nil {
		return nil, err
	}
	resolved := make(map[string][]byte, len(entries)+len(transaction.writes))
	for _, entry := range entries {
		resolved[string(entry.Key)] = clone(entry.Value)
	}
	for key, write := range transaction.writes {
		if !inRange([]byte(key), start, end) {
			continue
		}
		if write.delete {
			delete(resolved, key)
		} else {
			resolved[key] = clone(write.value)
		}
	}
	keys := make([]string, 0, len(resolved))
	for key := range resolved {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]kv.Entry, 0, len(keys))
	for _, key := range keys {
		result = append(result, kv.Entry{Key: []byte(key), Value: resolved[key]})
	}
	return result, nil
}

func (transaction *Transaction) Put(key, value []byte) error {
	if len(key) == 0 {
		return fmt.Errorf("mvcc: key cannot be empty")
	}
	transaction.mu.Lock()
	defer transaction.mu.Unlock()
	if transaction.closed {
		return ErrClosed
	}
	transaction.writes[string(key)] = pendingWrite{value: clone(value)}
	return nil
}

func (transaction *Transaction) Delete(key []byte) error {
	if len(key) == 0 {
		return fmt.Errorf("mvcc: key cannot be empty")
	}
	transaction.mu.Lock()
	defer transaction.mu.Unlock()
	if transaction.closed {
		return ErrClosed
	}
	transaction.writes[string(key)] = pendingWrite{delete: true}
	return nil
}

// Commit validates point reads, range reads, and writes against every commit
// after the transaction snapshot, then installs all versions atomically.
func (transaction *Transaction) Commit() (uint64, error) {
	transaction.mu.Lock()
	defer transaction.mu.Unlock()
	if transaction.closed {
		return 0, ErrClosed
	}
	transaction.manager.mu.Lock()
	defer transaction.manager.mu.Unlock()

	conflict, err := transaction.hasConflict()
	if err != nil {
		return 0, err
	}
	if conflict {
		transaction.closed = true
		return 0, ErrConflict
	}
	transaction.manager.clock++
	commitTimestamp := transaction.manager.clock
	keys := make([]string, 0, len(transaction.writes))
	for key := range transaction.writes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	mutations := make([]kv.Mutation, 0, len(keys))
	for _, key := range keys {
		write := transaction.writes[key]
		mutations = append(mutations, kv.Mutation{
			Key:   encodeVersionKey([]byte(key), commitTimestamp),
			Value: encodeVersionValue(write),
		})
	}
	if err := transaction.manager.store.Apply(mutations); err != nil {
		return 0, fmt.Errorf("mvcc: commit: %w", err)
	}
	transaction.closed = true
	return commitTimestamp, nil
}

func (transaction *Transaction) hasConflict() (bool, error) {
	checked := make(map[string]struct{}, len(transaction.readKeys)+len(transaction.writes))
	for key := range transaction.readKeys {
		checked[key] = struct{}{}
	}
	for key := range transaction.writes {
		checked[key] = struct{}{}
	}
	for key := range checked {
		latest, found, err := transaction.manager.latestTimestamp([]byte(key))
		if err != nil {
			return false, err
		}
		if found && latest > transaction.readTimestamp {
			return true, nil
		}
	}
	for _, current := range transaction.readSpans {
		changed, err := transaction.manager.spanChangedAfter(current.start, current.end, transaction.readTimestamp)
		if err != nil {
			return false, err
		}
		if changed {
			return true, nil
		}
	}
	return false, nil
}

// Rollback discards buffered intents. It is safe to call repeatedly.
func (transaction *Transaction) Rollback() {
	transaction.mu.Lock()
	transaction.closed = true
	transaction.writes = nil
	transaction.mu.Unlock()
}

func (manager *Manager) getAt(key []byte, timestamp uint64) ([]byte, bool, error) {
	prefix := versionPrefix(key)
	entries, err := manager.store.Scan(prefix, codec.PrefixEnd(prefix))
	if err != nil {
		return nil, false, err
	}
	for _, entry := range entries {
		_, committedAt, err := decodeVersionKey(entry.Key)
		if err != nil {
			return nil, false, err
		}
		if committedAt > timestamp {
			continue
		}
		value, tombstone, err := decodeVersionValue(entry.Value)
		if err != nil {
			return nil, false, err
		}
		if tombstone {
			return nil, false, nil
		}
		return value, true, nil
	}
	return nil, false, nil
}

func (manager *Manager) scanAt(start, end []byte, timestamp uint64) ([]kv.Entry, error) {
	physicalStart, physicalEnd := versionBounds(start, end)
	entries, err := manager.store.Scan(physicalStart, physicalEnd)
	if err != nil {
		return nil, err
	}
	result := make([]kv.Entry, 0)
	var previous string
	resolved := false
	for _, entry := range entries {
		logical, committedAt, err := decodeVersionKey(entry.Key)
		if err != nil {
			return nil, err
		}
		key := string(logical)
		if key != previous {
			previous, resolved = key, false
		}
		if resolved || committedAt > timestamp {
			continue
		}
		value, tombstone, err := decodeVersionValue(entry.Value)
		if err != nil {
			return nil, err
		}
		resolved = true
		if !tombstone {
			result = append(result, kv.Entry{Key: logical, Value: value})
		}
	}
	return result, nil
}

func (manager *Manager) latestTimestamp(key []byte) (uint64, bool, error) {
	prefix := versionPrefix(key)
	entries, err := manager.store.Scan(prefix, codec.PrefixEnd(prefix))
	if err != nil || len(entries) == 0 {
		return 0, false, err
	}
	_, timestamp, err := decodeVersionKey(entries[0].Key)
	return timestamp, err == nil, err
}

func (manager *Manager) spanChangedAfter(start, end []byte, timestamp uint64) (bool, error) {
	physicalStart, physicalEnd := versionBounds(start, end)
	entries, err := manager.store.Scan(physicalStart, physicalEnd)
	if err != nil {
		return false, err
	}
	previous := ""
	for _, entry := range entries {
		logical, committedAt, err := decodeVersionKey(entry.Key)
		if err != nil {
			return false, err
		}
		if string(logical) == previous {
			continue
		}
		previous = string(logical)
		if committedAt > timestamp {
			return true, nil
		}
	}
	return false, nil
}

func encodeVersionKey(logical []byte, timestamp uint64) []byte {
	result := versionScanBound(logical)
	result = append(result, 0, 0)
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], ^timestamp)
	return append(result, encoded[:]...)
}

func versionPrefix(logical []byte) []byte {
	result := versionScanBound(logical)
	return append(result, 0, 0)
}

func versionScanBound(logical []byte) []byte {
	result := make([]byte, 0, len(versionKeyPrefix)+len(logical)+2)
	result = append(result, versionKeyPrefix...)
	for _, current := range logical {
		if current == 0 {
			result = append(result, 0, 0xff)
		} else {
			result = append(result, current)
		}
	}
	return result
}

func versionBounds(start, end []byte) ([]byte, []byte) {
	physicalStart := append([]byte(nil), versionKeyPrefix...)
	if len(start) > 0 {
		physicalStart = versionScanBound(start)
	}
	physicalEnd := codec.PrefixEnd(versionKeyPrefix)
	if len(end) > 0 {
		physicalEnd = versionScanBound(end)
	}
	return physicalStart, physicalEnd
}

func decodeVersionKey(physical []byte) ([]byte, uint64, error) {
	if !bytes.HasPrefix(physical, versionKeyPrefix) {
		return nil, 0, fmt.Errorf("invalid MVCC key prefix")
	}
	encoded := physical[len(versionKeyPrefix):]
	logical := make([]byte, 0, len(encoded))
	position := 0
	for {
		if position >= len(encoded) {
			return nil, 0, fmt.Errorf("truncated MVCC key")
		}
		if encoded[position] != 0 {
			logical = append(logical, encoded[position])
			position++
			continue
		}
		if position+1 >= len(encoded) {
			return nil, 0, fmt.Errorf("truncated MVCC key escape")
		}
		switch encoded[position+1] {
		case 0:
			position += 2
			if len(encoded)-position != 8 {
				return nil, 0, fmt.Errorf("invalid MVCC timestamp suffix")
			}
			return logical, ^binary.BigEndian.Uint64(encoded[position:]), nil
		case 0xff:
			logical = append(logical, 0)
			position += 2
		default:
			return nil, 0, fmt.Errorf("invalid MVCC key escape")
		}
	}
}

func encodeVersionValue(write pendingWrite) []byte {
	if write.delete {
		return []byte{1}
	}
	return append([]byte{0}, write.value...)
}

func decodeVersionValue(encoded []byte) ([]byte, bool, error) {
	if len(encoded) == 0 || encoded[0] > 1 {
		return nil, false, fmt.Errorf("invalid MVCC value")
	}
	if encoded[0] == 1 {
		if len(encoded) != 1 {
			return nil, false, fmt.Errorf("MVCC tombstone contains a value")
		}
		return nil, true, nil
	}
	return clone(encoded[1:]), false, nil
}

func inRange(key, start, end []byte) bool {
	return (len(start) == 0 || bytes.Compare(key, start) >= 0) &&
		(len(end) == 0 || bytes.Compare(key, end) < 0)
}

func clone(value []byte) []byte {
	if value == nil {
		return nil
	}
	return append([]byte(nil), value...)
}
