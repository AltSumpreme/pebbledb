// Package indexstore maintains ordered secondary-index entries in the LSM.
package indexstore

import (
	"encoding/binary"
	"errors"
	"fmt"
	"pebbledb/catalog"
	"pebbledb/codec"
	"pebbledb/storage/kv"
	"pebbledb/types"
	"sync"
)

var ErrUniqueViolation = errors.New("indexstore: unique constraint violation")

var indexKeyPrefix = []byte{0x00, 'p', 'd', 'b', '-', 'i', 'd', 'x', 0x01}

type Store struct {
	mu    sync.Mutex
	store kv.Store
}

func New(store kv.Store) (*Store, error) {
	if store == nil {
		return nil, fmt.Errorf("indexstore: LSM store cannot be nil")
	}
	return &Store{store: store}, nil
}

// Check verifies every unique secondary index without modifying storage.
// ignorePrimary allows an UPDATE to retain its own existing entry.
func (store *Store) Check(table catalog.TableDescriptor, row codec.Row, ignorePrimary []types.Value) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.checkLocked(table, row, ignorePrimary)
}

// CheckBatch validates existing unique indexes and duplicate values within a
// multi-row INSERT before any row is written.
func (store *Store) CheckBatch(table catalog.TableDescriptor, rows []codec.Row) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	seen := make(map[string]struct{})
	for _, row := range rows {
		if err := store.checkLocked(table, row, nil); err != nil {
			return err
		}
		for _, index := range table.Indexes {
			if !index.Unique {
				continue
			}
			values, err := indexValues(table, index, row)
			if err != nil {
				return err
			}
			if containsNull(values) {
				continue
			}
			encoded, err := codec.EncodeKey(values...)
			if err != nil {
				return err
			}
			key := fmt.Sprintf("%d:%s", index.ID, encoded)
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("%w: index %q", ErrUniqueViolation, index.Name)
			}
			seen[key] = struct{}{}
		}
	}
	return nil
}

func (store *Store) checkLocked(table catalog.TableDescriptor, row codec.Row, ignorePrimary []types.Value) error {
	for _, index := range table.Indexes {
		if !index.Unique {
			continue
		}
		indexValues, err := indexValues(table, index, row)
		if err != nil {
			return err
		}
		// PostgreSQL permits multiple NULLs in a normal UNIQUE index.
		if containsNull(indexValues) {
			continue
		}
		key, _, err := entry(table, index, row)
		if err != nil {
			return err
		}
		payload, found, err := store.store.Get(key)
		if err != nil || !found {
			continue
		}
		_, existingPrimary, err := decodePayload(payload)
		if err != nil {
			return err
		}
		if ignorePrimary != nil {
			ignored, err := codec.EncodeKey(ignorePrimary...)
			if err != nil {
				return err
			}
			if string(existingPrimary) == string(ignored) {
				continue
			}
		}
		return fmt.Errorf("%w: index %q", ErrUniqueViolation, index.Name)
	}
	return nil
}

func (store *Store) Add(table catalog.TableDescriptor, row codec.Row) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if err := store.checkLocked(table, row, nil); err != nil {
		return err
	}
	var installed [][]byte
	for _, index := range table.Indexes {
		key, payload, err := entry(table, index, row)
		if err != nil {
			return err
		}
		if err := store.store.Put(key, payload); err != nil {
			for _, rollbackKey := range installed {
				_ = store.store.Delete(rollbackKey)
			}
			return err
		}
		installed = append(installed, key)
	}
	return nil
}

func (store *Store) Remove(table catalog.TableDescriptor, row codec.Row) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, index := range table.Indexes {
		key, _, err := entry(table, index, row)
		if err != nil {
			return err
		}
		if err := store.store.Delete(key); err != nil {
			return err
		}
	}
	return nil
}

// Backfill validates and writes one newly-created index for existing rows.
func (store *Store) Backfill(table catalog.TableDescriptor, index catalog.IndexDescriptor, rows []codec.Row) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	table.Indexes = []catalog.IndexDescriptor{index}
	var installed [][]byte
	for _, row := range rows {
		if err := store.checkLocked(table, row, nil); err != nil {
			for _, key := range installed {
				_ = store.store.Delete(key)
			}
			return err
		}
		key, payload, err := entry(table, index, row)
		if err != nil {
			return err
		}
		if err := store.store.Put(key, payload); err != nil {
			for _, rollbackKey := range installed {
				_ = store.store.Delete(rollbackKey)
			}
			return err
		}
		installed = append(installed, key)
	}
	return nil
}

// Lookup returns primary keys for an equality lookup, in index-key order.
func (store *Store) Lookup(table catalog.TableDescriptor, index catalog.IndexDescriptor, values []types.Value) ([][]types.Value, error) {
	prefix, err := valuePrefix(table.ID, index.ID, values)
	if err != nil {
		return nil, err
	}
	entries, err := store.store.Scan(prefix, codec.PrefixEnd(prefix))
	if err != nil {
		return nil, err
	}
	result := make([][]types.Value, 0, len(entries))
	for _, current := range entries {
		_, primaryEncoded, err := decodePayload(current.Value)
		if err != nil {
			return nil, err
		}
		primary, err := codec.DecodeKey(primaryEncoded)
		if err != nil {
			return nil, err
		}
		result = append(result, primary)
	}
	return result, nil
}

// IndexStats derives exact entry and distinct-key counts from the current index.
func (store *Store) IndexStats(table catalog.TableDescriptor, index catalog.IndexDescriptor) (entries uint64, distinct uint64, err error) {
	prefix := IndexPrefix(table.ID, index.ID)
	stored, err := store.store.Scan(prefix, codec.PrefixEnd(prefix))
	if err != nil {
		return 0, 0, err
	}
	distinctKeys := make(map[string]struct{})
	for _, current := range stored {
		indexEncoded, _, err := decodePayload(current.Value)
		if err != nil {
			return 0, 0, err
		}
		distinctKeys[string(indexEncoded)] = struct{}{}
	}
	return uint64(len(stored)), uint64(len(distinctKeys)), nil
}

func IndexPrefix(tableID, indexID catalog.DescriptorID) []byte {
	prefix := append([]byte(nil), indexKeyPrefix...)
	var raw [16]byte
	binary.BigEndian.PutUint64(raw[:8], uint64(tableID))
	binary.BigEndian.PutUint64(raw[8:], uint64(indexID))
	return append(prefix, raw[:]...)
}

func valuePrefix(tableID, indexID catalog.DescriptorID, values []types.Value) ([]byte, error) {
	encoded, err := codec.EncodeKey(values...)
	if err != nil {
		return nil, err
	}
	return append(IndexPrefix(tableID, indexID), encoded...), nil
}

func entry(table catalog.TableDescriptor, index catalog.IndexDescriptor, row codec.Row) ([]byte, []byte, error) {
	indexedValues, err := indexValues(table, index, row)
	if err != nil {
		return nil, nil, err
	}
	primaryValues := make([]types.Value, 0, len(table.Schema.PrimaryKey))
	for _, columnID := range table.Schema.PrimaryKey {
		primaryValues = append(primaryValues, row.Values[columnID])
	}
	indexEncoded, err := codec.EncodeKey(indexedValues...)
	if err != nil {
		return nil, nil, err
	}
	primaryEncoded, err := codec.EncodeKey(primaryValues...)
	if err != nil {
		return nil, nil, err
	}
	key := append(IndexPrefix(table.ID, index.ID), indexEncoded...)
	if !index.Unique || containsNull(indexedValues) {
		key = append(key, primaryEncoded...)
	}
	return key, encodePayload(indexEncoded, primaryEncoded), nil
}

func indexValues(table catalog.TableDescriptor, index catalog.IndexDescriptor, row codec.Row) ([]types.Value, error) {
	values := make([]types.Value, 0, len(index.ColumnIDs))
	for _, columnID := range index.ColumnIDs {
		value, exists := row.Values[columnID]
		if !exists {
			return nil, fmt.Errorf("indexstore: row is missing index column %d", columnID)
		}
		values = append(values, value)
	}
	return values, nil
}

func containsNull(values []types.Value) bool {
	for _, value := range values {
		if value.IsNull() {
			return true
		}
	}
	return false
}

func encodePayload(indexEncoded, primaryEncoded []byte) []byte {
	result := make([]byte, 4, 4+len(indexEncoded)+len(primaryEncoded))
	binary.LittleEndian.PutUint32(result, uint32(len(indexEncoded)))
	result = append(result, indexEncoded...)
	return append(result, primaryEncoded...)
}

func decodePayload(payload []byte) ([]byte, []byte, error) {
	if len(payload) < 6 {
		return nil, nil, fmt.Errorf("indexstore: index payload is truncated")
	}
	indexLength := binary.LittleEndian.Uint32(payload[:4])
	if indexLength == 0 || uint64(indexLength) > uint64(len(payload)-5) {
		return nil, nil, fmt.Errorf("indexstore: invalid index payload length")
	}
	return payload[4 : 4+indexLength], payload[4+indexLength:], nil
}
