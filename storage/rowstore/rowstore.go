// Package rowstore maps relational primary keys and typed rows onto the LSM.
package rowstore

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

var (
	ErrDuplicateKey = errors.New("rowstore: duplicate primary key")
	ErrNoPrimaryKey = errors.New("rowstore: table has no primary key")
	ErrRowNotFound  = errors.New("rowstore: row not found")
)

var rowKeyPrefix = []byte{0x00, 'p', 'd', 'b', '-', 'r', 'o', 'w', 0x01}

type StoredRow struct {
	Key []byte
	Row codec.Row
}

type Store struct {
	mu    sync.Mutex
	store kv.Store
}

func New(store kv.Store) (*Store, error) {
	if store == nil {
		return nil, fmt.Errorf("rowstore: LSM store cannot be nil")
	}
	return &Store{store: store}, nil
}

// Insert rejects an existing primary key and durably writes one encoded row.
func (store *Store) Insert(table catalog.TableDescriptor, row codec.Row) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	key, value, err := encode(table, row)
	if err != nil {
		return err
	}
	_, found, err := store.store.Get(key)
	if err != nil {
		return err
	}
	if found {
		return fmt.Errorf("%w on table %q", ErrDuplicateKey, table.Name)
	}
	return store.store.Put(key, value)
}

// Replace updates an existing row. Primary-key changes must be expressed as a
// Delete followed by Insert once transaction batches are available.
func (store *Store) Replace(table catalog.TableDescriptor, row codec.Row) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	key, value, err := encode(table, row)
	if err != nil {
		return err
	}
	_, found, err := store.store.Get(key)
	if err != nil {
		return err
	}
	if !found {
		return ErrRowNotFound
	}
	return store.store.Put(key, value)
}

func (store *Store) Get(table catalog.TableDescriptor, primaryKey []types.Value) (codec.Row, error) {
	key, err := Key(table, primaryKey)
	if err != nil {
		return codec.Row{}, err
	}
	value, found, err := store.store.Get(key)
	if err != nil {
		return codec.Row{}, err
	}
	if !found {
		return codec.Row{}, ErrRowNotFound
	}
	return codec.DecodeRow(value)
}

func (store *Store) Delete(table catalog.TableDescriptor, row codec.Row) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	key, err := KeyFromRow(table, row)
	if err != nil {
		return err
	}
	_, found, err := store.store.Get(key)
	if err != nil {
		return err
	}
	if !found {
		return ErrRowNotFound
	}
	return store.store.Delete(key)
}

// Scan returns rows in primary-key order.
func (store *Store) Scan(table catalog.TableDescriptor) ([]StoredRow, error) {
	prefix := TablePrefix(table.ID)
	entries, err := store.store.Scan(prefix, codec.PrefixEnd(prefix))
	if err != nil {
		return nil, err
	}
	rows := make([]StoredRow, 0, len(entries))
	for _, entry := range entries {
		row, err := codec.DecodeRow(entry.Value)
		if err != nil {
			return nil, fmt.Errorf("rowstore: decode table %q row: %w", table.Name, err)
		}
		rows = append(rows, StoredRow{Key: entry.Key, Row: row})
	}
	return rows, nil
}

func KeyFromRow(table catalog.TableDescriptor, row codec.Row) ([]byte, error) {
	if len(table.Schema.PrimaryKey) == 0 {
		return nil, ErrNoPrimaryKey
	}
	primaryKey := make([]types.Value, 0, len(table.Schema.PrimaryKey))
	for _, columnID := range table.Schema.PrimaryKey {
		value, exists := row.Values[columnID]
		if !exists {
			return nil, fmt.Errorf("rowstore: row is missing primary-key column %d", columnID)
		}
		if value.IsNull() {
			return nil, fmt.Errorf("rowstore: primary-key column %d is NULL", columnID)
		}
		primaryKey = append(primaryKey, value)
	}
	return Key(table, primaryKey)
}

func Key(table catalog.TableDescriptor, primaryKey []types.Value) ([]byte, error) {
	if len(table.Schema.PrimaryKey) == 0 {
		return nil, ErrNoPrimaryKey
	}
	if len(primaryKey) != len(table.Schema.PrimaryKey) {
		return nil, fmt.Errorf("rowstore: got %d primary-key values, want %d", len(primaryKey), len(table.Schema.PrimaryKey))
	}
	columnByID := make(map[uint32]codec.ColumnDescriptor, len(table.Schema.Columns))
	for _, column := range table.Schema.Columns {
		columnByID[column.ID] = column
	}
	for index, columnID := range table.Schema.PrimaryKey {
		column := columnByID[columnID]
		if primaryKey[index].IsNull() || primaryKey[index].Type() != column.Type {
			return nil, fmt.Errorf("rowstore: primary-key value %d must be non-null %s", index+1, column.Type)
		}
	}
	encodedPrimary, err := codec.EncodeKey(primaryKey...)
	if err != nil {
		return nil, err
	}
	return append(TablePrefix(table.ID), encodedPrimary...), nil
}

func TablePrefix(tableID catalog.DescriptorID) []byte {
	prefix := append([]byte(nil), rowKeyPrefix...)
	var rawID [8]byte
	binary.BigEndian.PutUint64(rawID[:], uint64(tableID))
	return append(prefix, rawID[:]...)
}

func encode(table catalog.TableDescriptor, row codec.Row) ([]byte, []byte, error) {
	if row.SchemaVersion != table.Schema.Version {
		return nil, nil, fmt.Errorf("rowstore: row schema version %d does not match table version %d", row.SchemaVersion, table.Schema.Version)
	}
	for _, column := range table.Schema.Columns {
		value, exists := row.Values[column.ID]
		if !exists {
			return nil, nil, fmt.Errorf("rowstore: row is missing column %q", column.Name)
		}
		if value.Type() != column.Type {
			return nil, nil, fmt.Errorf("rowstore: column %q has %s value, expected %s", column.Name, value.Type(), column.Type)
		}
		if value.IsNull() && !column.Nullable {
			return nil, nil, fmt.Errorf("rowstore: column %q violates NOT NULL", column.Name)
		}
	}
	if len(row.Values) != len(table.Schema.Columns) {
		return nil, nil, fmt.Errorf("rowstore: row contains unknown columns")
	}
	key, err := KeyFromRow(table, row)
	if err != nil {
		return nil, nil, err
	}
	value, err := codec.EncodeRow(row)
	return key, value, err
}
