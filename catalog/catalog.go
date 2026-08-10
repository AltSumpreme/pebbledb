package catalog

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"pebbledb/codec"
	"pebbledb/storage/kv"
	"sort"
	"sync"
)

var (
	ErrNotFound      = errors.New("catalog: descriptor not found")
	ErrAlreadyExists = errors.New("catalog: descriptor already exists")
	ErrDependency    = errors.New("catalog: descriptor has dependent objects")
)

var catalogKeyPrefix = []byte{0x00, 'p', 'd', 'b', '-', 'c', 'a', 't', 0x01}

// Catalog stores versioned descriptors in an LSM. Each table descriptor is one
// atomic storage value, so its columns, indexes, constraints, and partitions
// cannot be partially updated.
type Catalog struct {
	mu    sync.RWMutex
	store kv.Store
}

func New(store kv.Store) (*Catalog, error) {
	if store == nil {
		return nil, fmt.Errorf("catalog: store cannot be nil")
	}
	return &Catalog{store: store}, nil
}

// EnsureDefaults idempotently creates the pebbledb database and public schema.
func (catalog *Catalog) EnsureDefaults() (DatabaseDescriptor, SchemaDescriptor, error) {
	database, err := catalog.GetDatabase("pebbledb")
	if errors.Is(err, ErrNotFound) {
		database, err = catalog.CreateDatabase("pebbledb")
		if errors.Is(err, ErrAlreadyExists) {
			database, err = catalog.GetDatabase("pebbledb")
		}
	}
	if err != nil {
		return DatabaseDescriptor{}, SchemaDescriptor{}, err
	}
	schema, err := catalog.GetSchema(database.ID, "public")
	if errors.Is(err, ErrNotFound) {
		schema, err = catalog.CreateSchema(database.ID, "public")
		if errors.Is(err, ErrAlreadyExists) {
			schema, err = catalog.GetSchema(database.ID, "public")
		}
	}
	return database, schema, err
}

func (catalog *Catalog) CreateDatabase(name string) (DatabaseDescriptor, error) {
	normalized, err := normalizeName(name)
	if err != nil {
		return DatabaseDescriptor{}, err
	}
	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	if _, err := catalog.findDatabaseLocked(normalized); err == nil {
		return DatabaseDescriptor{}, fmt.Errorf("%w: database %q", ErrAlreadyExists, normalized)
	} else if !errors.Is(err, ErrNotFound) {
		return DatabaseDescriptor{}, err
	}
	id, err := catalog.newRecordIDLocked(databaseKind)
	if err != nil {
		return DatabaseDescriptor{}, err
	}
	descriptor := DatabaseDescriptor{ID: id, Name: normalized}
	if err := catalog.putLocked(databaseKind, id, descriptor); err != nil {
		return DatabaseDescriptor{}, err
	}
	return descriptor, nil
}

func (catalog *Catalog) GetDatabase(name string) (DatabaseDescriptor, error) {
	normalized, err := normalizeName(name)
	if err != nil {
		return DatabaseDescriptor{}, err
	}
	catalog.mu.RLock()
	defer catalog.mu.RUnlock()
	return catalog.findDatabaseLocked(normalized)
}

func (catalog *Catalog) ListDatabases() ([]DatabaseDescriptor, error) {
	catalog.mu.RLock()
	defer catalog.mu.RUnlock()
	return catalog.listDatabasesLocked()
}

func (catalog *Catalog) DropDatabase(id DescriptorID) error {
	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	if _, err := catalog.getDatabaseByIDLocked(id); err != nil {
		return err
	}
	schemas, err := catalog.listSchemasLocked(id)
	if err != nil {
		return err
	}
	if len(schemas) > 0 {
		return fmt.Errorf("%w: database has schemas", ErrDependency)
	}
	return catalog.store.Delete(descriptorKey(databaseKind, id))
}

func (catalog *Catalog) CreateSchema(databaseID DescriptorID, name string) (SchemaDescriptor, error) {
	normalized, err := normalizeName(name)
	if err != nil {
		return SchemaDescriptor{}, err
	}
	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	if _, err := catalog.getDatabaseByIDLocked(databaseID); err != nil {
		return SchemaDescriptor{}, err
	}
	if _, err := catalog.findSchemaLocked(databaseID, normalized); err == nil {
		return SchemaDescriptor{}, fmt.Errorf("%w: schema %q", ErrAlreadyExists, normalized)
	} else if !errors.Is(err, ErrNotFound) {
		return SchemaDescriptor{}, err
	}
	id, err := catalog.newRecordIDLocked(schemaKind)
	if err != nil {
		return SchemaDescriptor{}, err
	}
	descriptor := SchemaDescriptor{ID: id, DatabaseID: databaseID, Name: normalized}
	if err := catalog.putLocked(schemaKind, id, descriptor); err != nil {
		return SchemaDescriptor{}, err
	}
	return descriptor, nil
}

func (catalog *Catalog) GetSchema(databaseID DescriptorID, name string) (SchemaDescriptor, error) {
	normalized, err := normalizeName(name)
	if err != nil {
		return SchemaDescriptor{}, err
	}
	catalog.mu.RLock()
	defer catalog.mu.RUnlock()
	return catalog.findSchemaLocked(databaseID, normalized)
}

func (catalog *Catalog) ListSchemas(databaseID DescriptorID) ([]SchemaDescriptor, error) {
	catalog.mu.RLock()
	defer catalog.mu.RUnlock()
	return catalog.listSchemasLocked(databaseID)
}

func (catalog *Catalog) DropSchema(id DescriptorID) error {
	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	if _, err := catalog.getSchemaByIDLocked(id); err != nil {
		return err
	}
	tables, err := catalog.listTablesLocked(id)
	if err != nil {
		return err
	}
	if len(tables) > 0 {
		return fmt.Errorf("%w: schema has tables", ErrDependency)
	}
	return catalog.store.Delete(descriptorKey(schemaKind, id))
}

func (catalog *Catalog) CreateTable(schemaID DescriptorID, name string, columns []codec.ColumnDescriptor, primaryKey []uint32) (TableDescriptor, error) {
	normalized, err := normalizeName(name)
	if err != nil {
		return TableDescriptor{}, err
	}
	normalizedColumns := append([]codec.ColumnDescriptor(nil), columns...)
	for index := range normalizedColumns {
		normalizedColumns[index].Name, err = normalizeName(normalizedColumns[index].Name)
		if err != nil {
			return TableDescriptor{}, fmt.Errorf("catalog: column %d: %w", index+1, err)
		}
	}
	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	if _, err := catalog.getSchemaByIDLocked(schemaID); err != nil {
		return TableDescriptor{}, err
	}
	if _, err := catalog.findTableLocked(schemaID, normalized); err == nil {
		return TableDescriptor{}, fmt.Errorf("%w: table %q", ErrAlreadyExists, normalized)
	} else if !errors.Is(err, ErrNotFound) {
		return TableDescriptor{}, err
	}
	for _, primaryID := range primaryKey {
		for _, column := range normalizedColumns {
			if column.ID == primaryID && column.Nullable {
				return TableDescriptor{}, fmt.Errorf("catalog: primary-key column %q cannot be nullable", column.Name)
			}
		}
	}
	tableID, err := catalog.newRecordIDLocked(tableKind)
	if err != nil {
		return TableDescriptor{}, err
	}
	descriptor := TableDescriptor{
		ID:       tableID,
		SchemaID: schemaID,
		Name:     normalized,
		Version:  1,
		Schema: codec.TableSchema{
			Version:    1,
			Columns:    normalizedColumns,
			PrimaryKey: append([]uint32(nil), primaryKey...),
		},
	}
	if len(primaryKey) > 0 {
		constraintID, err := randomDescriptorID()
		if err != nil {
			return TableDescriptor{}, err
		}
		descriptor.Constraints = []ConstraintDescriptor{{ID: constraintID, Name: normalized + "_pkey", Kind: PrimaryKeyConstraint, ColumnIDs: append([]uint32(nil), primaryKey...)}}
	}
	if err := validateTable(descriptor); err != nil {
		return TableDescriptor{}, err
	}
	if err := catalog.putLocked(tableKind, tableID, descriptor); err != nil {
		return TableDescriptor{}, err
	}
	return descriptor, nil
}

func (catalog *Catalog) GetTable(schemaID DescriptorID, name string) (TableDescriptor, error) {
	normalized, err := normalizeName(name)
	if err != nil {
		return TableDescriptor{}, err
	}
	catalog.mu.RLock()
	defer catalog.mu.RUnlock()
	return catalog.findTableLocked(schemaID, normalized)
}

// DescribeTable returns the complete table descriptor used by DESCRIBE/\d-style
// clients: columns, primary key, indexes, constraints, and partitions.
func (catalog *Catalog) DescribeTable(schemaID DescriptorID, name string) (TableDescriptor, error) {
	return catalog.GetTable(schemaID, name)
}

func (catalog *Catalog) GetTableByID(id DescriptorID) (TableDescriptor, error) {
	catalog.mu.RLock()
	defer catalog.mu.RUnlock()
	return catalog.getTableByIDLocked(id)
}

func (catalog *Catalog) ListTables(schemaID DescriptorID) ([]TableDescriptor, error) {
	catalog.mu.RLock()
	defer catalog.mu.RUnlock()
	return catalog.listTablesLocked(schemaID)
}

func (catalog *Catalog) DropTable(id DescriptorID) error {
	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	if _, err := catalog.getTableByIDLocked(id); err != nil {
		return err
	}
	entries, err := catalog.scanKindLocked(tableKind)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		var candidate TableDescriptor
		if err := decodeRecord(entry.Value, tableKind, &candidate); err != nil {
			return err
		}
		if err := validateTable(candidate); err != nil {
			return err
		}
		if candidate.ID == id {
			continue
		}
		for _, constraint := range candidate.Constraints {
			if constraint.Kind == ForeignKeyConstraint && constraint.ReferencedTableID == id {
				return fmt.Errorf("%w: table is referenced by constraint %q on table %q", ErrDependency, constraint.Name, candidate.Name)
			}
		}
	}
	return catalog.store.Delete(descriptorKey(tableKind, id))
}

func (catalog *Catalog) AddIndex(tableID DescriptorID, name string, columnIDs []uint32, unique bool) (TableDescriptor, error) {
	normalized, err := normalizeName(name)
	if err != nil {
		return TableDescriptor{}, err
	}
	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	table, err := catalog.getTableByIDLocked(tableID)
	if err != nil {
		return TableDescriptor{}, err
	}
	for _, index := range table.Indexes {
		if index.Name == normalized {
			return TableDescriptor{}, fmt.Errorf("%w: index %q", ErrAlreadyExists, normalized)
		}
	}
	id, err := randomDescriptorID()
	if err != nil {
		return TableDescriptor{}, err
	}
	table.Indexes = append(table.Indexes, IndexDescriptor{ID: id, Name: normalized, ColumnIDs: append([]uint32(nil), columnIDs...), Unique: unique})
	table.Version++
	return catalog.storeTableLocked(table)
}

func (catalog *Catalog) DropIndex(tableID, indexID DescriptorID) (TableDescriptor, error) {
	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	table, err := catalog.getTableByIDLocked(tableID)
	if err != nil {
		return TableDescriptor{}, err
	}
	position := -1
	for index := range table.Indexes {
		if table.Indexes[index].ID == indexID {
			position = index
			break
		}
	}
	if position < 0 {
		return TableDescriptor{}, fmt.Errorf("%w: index ID %d", ErrNotFound, indexID)
	}
	table.Indexes = append(table.Indexes[:position], table.Indexes[position+1:]...)
	table.Version++
	return catalog.storeTableLocked(table)
}

func (catalog *Catalog) AddConstraint(tableID DescriptorID, constraint ConstraintDescriptor) (TableDescriptor, error) {
	normalized, err := normalizeName(constraint.Name)
	if err != nil {
		return TableDescriptor{}, err
	}
	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	table, err := catalog.getTableByIDLocked(tableID)
	if err != nil {
		return TableDescriptor{}, err
	}
	for _, existing := range table.Constraints {
		if existing.Name == normalized {
			return TableDescriptor{}, fmt.Errorf("%w: constraint %q", ErrAlreadyExists, normalized)
		}
	}
	constraint.Name = normalized
	constraint.ColumnIDs = append([]uint32(nil), constraint.ColumnIDs...)
	constraint.ReferencedColumnIDs = append([]uint32(nil), constraint.ReferencedColumnIDs...)
	if constraint.ID == 0 {
		constraint.ID, err = randomDescriptorID()
		if err != nil {
			return TableDescriptor{}, err
		}
	}
	if constraint.Kind == ForeignKeyConstraint {
		referenced, err := catalog.getTableByIDLocked(constraint.ReferencedTableID)
		if err != nil {
			return TableDescriptor{}, fmt.Errorf("catalog: referenced table: %w", err)
		}
		referencedColumns := make(map[uint32]struct{}, len(referenced.Schema.Columns))
		for _, column := range referenced.Schema.Columns {
			referencedColumns[column.ID] = struct{}{}
		}
		if err := validateColumnReferences(referencedColumns, constraint.ReferencedColumnIDs); err != nil {
			return TableDescriptor{}, fmt.Errorf("catalog: referenced columns: %w", err)
		}
	}
	table.Constraints = append(table.Constraints, constraint)
	table.Version++
	return catalog.storeTableLocked(table)
}

func (catalog *Catalog) AddPartition(tableID DescriptorID, startKey, endKey []byte) (TableDescriptor, error) {
	catalog.mu.Lock()
	defer catalog.mu.Unlock()
	table, err := catalog.getTableByIDLocked(tableID)
	if err != nil {
		return TableDescriptor{}, err
	}
	id, err := randomDescriptorID()
	if err != nil {
		return TableDescriptor{}, err
	}
	table.Partitions = append(table.Partitions, PartitionDescriptor{ID: id, StartKey: append([]byte(nil), startKey...), EndKey: append([]byte(nil), endKey...)})
	table.Partitions = sortedPartitions(table.Partitions)
	table.Version++
	return catalog.storeTableLocked(table)
}

func (catalog *Catalog) storeTableLocked(table TableDescriptor) (TableDescriptor, error) {
	if err := validateTable(table); err != nil {
		return TableDescriptor{}, err
	}
	if err := catalog.putLocked(tableKind, table.ID, table); err != nil {
		return TableDescriptor{}, err
	}
	return table, nil
}

func (catalog *Catalog) findDatabaseLocked(name string) (DatabaseDescriptor, error) {
	descriptors, err := catalog.listDatabasesLocked()
	if err != nil {
		return DatabaseDescriptor{}, err
	}
	for _, descriptor := range descriptors {
		if descriptor.Name == name {
			return descriptor, nil
		}
	}
	return DatabaseDescriptor{}, fmt.Errorf("%w: database %q", ErrNotFound, name)
}

func (catalog *Catalog) findSchemaLocked(databaseID DescriptorID, name string) (SchemaDescriptor, error) {
	descriptors, err := catalog.listSchemasLocked(databaseID)
	if err != nil {
		return SchemaDescriptor{}, err
	}
	for _, descriptor := range descriptors {
		if descriptor.Name == name {
			return descriptor, nil
		}
	}
	return SchemaDescriptor{}, fmt.Errorf("%w: schema %q", ErrNotFound, name)
}

func (catalog *Catalog) findTableLocked(schemaID DescriptorID, name string) (TableDescriptor, error) {
	descriptors, err := catalog.listTablesLocked(schemaID)
	if err != nil {
		return TableDescriptor{}, err
	}
	for _, descriptor := range descriptors {
		if descriptor.Name == name {
			return descriptor, nil
		}
	}
	return TableDescriptor{}, fmt.Errorf("%w: table %q", ErrNotFound, name)
}

func (catalog *Catalog) listDatabasesLocked() ([]DatabaseDescriptor, error) {
	entries, err := catalog.scanKindLocked(databaseKind)
	if err != nil {
		return nil, err
	}
	result := make([]DatabaseDescriptor, 0, len(entries))
	for _, entry := range entries {
		var descriptor DatabaseDescriptor
		if err := decodeRecord(entry.Value, databaseKind, &descriptor); err != nil {
			return nil, err
		}
		if err := validateDatabase(descriptor); err != nil {
			return nil, err
		}
		result = append(result, descriptor)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

func (catalog *Catalog) listSchemasLocked(databaseID DescriptorID) ([]SchemaDescriptor, error) {
	entries, err := catalog.scanKindLocked(schemaKind)
	if err != nil {
		return nil, err
	}
	var result []SchemaDescriptor
	for _, entry := range entries {
		var descriptor SchemaDescriptor
		if err := decodeRecord(entry.Value, schemaKind, &descriptor); err != nil {
			return nil, err
		}
		if err := validateSchema(descriptor); err != nil {
			return nil, err
		}
		if descriptor.DatabaseID == databaseID {
			result = append(result, descriptor)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

func (catalog *Catalog) listTablesLocked(schemaID DescriptorID) ([]TableDescriptor, error) {
	entries, err := catalog.scanKindLocked(tableKind)
	if err != nil {
		return nil, err
	}
	var result []TableDescriptor
	for _, entry := range entries {
		var descriptor TableDescriptor
		if err := decodeRecord(entry.Value, tableKind, &descriptor); err != nil {
			return nil, err
		}
		if err := validateTable(descriptor); err != nil {
			return nil, err
		}
		if descriptor.SchemaID == schemaID {
			result = append(result, descriptor)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

func (catalog *Catalog) getDatabaseByIDLocked(id DescriptorID) (DatabaseDescriptor, error) {
	var descriptor DatabaseDescriptor
	if err := catalog.getLocked(databaseKind, id, &descriptor); err != nil {
		return DatabaseDescriptor{}, err
	}
	return descriptor, validateDatabase(descriptor)
}

func (catalog *Catalog) getSchemaByIDLocked(id DescriptorID) (SchemaDescriptor, error) {
	var descriptor SchemaDescriptor
	if err := catalog.getLocked(schemaKind, id, &descriptor); err != nil {
		return SchemaDescriptor{}, err
	}
	return descriptor, validateSchema(descriptor)
}

func (catalog *Catalog) getTableByIDLocked(id DescriptorID) (TableDescriptor, error) {
	var descriptor TableDescriptor
	if err := catalog.getLocked(tableKind, id, &descriptor); err != nil {
		return TableDescriptor{}, err
	}
	return descriptor, validateTable(descriptor)
}

func (catalog *Catalog) getLocked(kind descriptorKind, id DescriptorID, destination any) error {
	value, found, err := catalog.store.Get(descriptorKey(kind, id))
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("%w: kind %d ID %d", ErrNotFound, kind, id)
	}
	return decodeRecord(value, kind, destination)
}

func (catalog *Catalog) putLocked(kind descriptorKind, id DescriptorID, descriptor any) error {
	encoded, err := encodeRecord(kind, descriptor)
	if err != nil {
		return err
	}
	return catalog.store.Put(descriptorKey(kind, id), encoded)
}

func (catalog *Catalog) scanKindLocked(kind descriptorKind) ([]kv.Entry, error) {
	prefix := descriptorPrefix(kind)
	return catalog.store.Scan(prefix, codec.PrefixEnd(prefix))
}

func (catalog *Catalog) newRecordIDLocked(kind descriptorKind) (DescriptorID, error) {
	for attempt := 0; attempt < 100; attempt++ {
		id, err := randomDescriptorID()
		if err != nil {
			return 0, err
		}
		_, found, err := catalog.store.Get(descriptorKey(kind, id))
		if err != nil {
			return 0, err
		}
		if !found {
			return id, nil
		}
	}
	return 0, fmt.Errorf("catalog: could not allocate a unique descriptor ID")
}

func randomDescriptorID() (DescriptorID, error) {
	for {
		var raw [8]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return 0, fmt.Errorf("catalog: generate descriptor ID: %w", err)
		}
		if id := DescriptorID(binary.BigEndian.Uint64(raw[:])); id != 0 {
			return id, nil
		}
	}
}

func descriptorPrefix(kind descriptorKind) []byte {
	prefix := append([]byte(nil), catalogKeyPrefix...)
	return append(prefix, byte(kind))
}

func descriptorKey(kind descriptorKind, id DescriptorID) []byte {
	key := descriptorPrefix(kind)
	var rawID [8]byte
	binary.BigEndian.PutUint64(rawID[:], uint64(id))
	return append(key, rawID[:]...)
}
