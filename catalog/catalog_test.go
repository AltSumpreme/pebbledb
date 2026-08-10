package catalog

import (
	"bytes"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"pebbledb/codec"
	"pebbledb/storage/lsm"
	"pebbledb/types"
)

func TestCatalogPersistsCompleteTableDescriptor(t *testing.T) {
	directory := t.TempDir()
	store, err := lsm.Open(directory)
	if err != nil {
		t.Fatalf("open LSM: %v", err)
	}
	catalog, _ := New(store)
	database, schema, err := catalog.EnsureDefaults()
	if err != nil {
		t.Fatalf("defaults: %v", err)
	}
	secondDatabase, secondSchema, err := catalog.EnsureDefaults()
	if err != nil || secondDatabase.ID != database.ID || secondSchema.ID != schema.ID {
		t.Fatalf("defaults were not idempotent: (%+v, %+v, %v)", secondDatabase, secondSchema, err)
	}

	priceType, _ := types.DecimalType(12, 2)
	table, err := catalog.CreateTable(schema.ID, "Products", []codec.ColumnDescriptor{
		{ID: 1, Name: "ID", Type: types.BigIntType()},
		{ID: 2, Name: "Name", Type: types.TextType()},
		{ID: 3, Name: "Price", Type: priceType},
	}, []uint32{1})
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	if table.Name != "products" || table.Schema.Columns[1].Name != "name" {
		t.Fatalf("identifiers were not normalized: %+v", table)
	}
	table, err = catalog.AddIndex(table.ID, "products_name_idx", []uint32{2}, true)
	if err != nil {
		t.Fatalf("add index: %v", err)
	}
	table, err = catalog.AddConstraint(table.ID, ConstraintDescriptor{Name: "positive_price", Kind: CheckConstraint, Expression: "price >= 0"})
	if err != nil {
		t.Fatalf("add constraint: %v", err)
	}
	table, err = catalog.AddPartition(table.ID, nil, []byte("m"))
	if err != nil {
		t.Fatalf("add first partition: %v", err)
	}
	table, err = catalog.AddPartition(table.ID, []byte("m"), nil)
	if err != nil {
		t.Fatalf("add second partition: %v", err)
	}
	if table.Version != 5 {
		t.Fatalf("table descriptor version = %d, want 5", table.Version)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	store, err = lsm.Open(directory)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	catalog, _ = New(store)
	reloaded, err := catalog.GetTable(schema.ID, "PRODUCTS")
	if err != nil {
		t.Fatalf("get reloaded table: %v", err)
	}
	if reloaded.ID != table.ID || reloaded.Version != table.Version || len(reloaded.Indexes) != 1 || len(reloaded.Constraints) != 2 || len(reloaded.Partitions) != 2 {
		t.Fatalf("incomplete reloaded descriptor: %+v", reloaded)
	}
}

func TestCatalogHierarchyListingAndDependencyChecks(t *testing.T) {
	catalog, store := openTestCatalog(t)
	database, err := catalog.CreateDatabase("Shop")
	if err != nil {
		t.Fatalf("create database: %v", err)
	}
	if _, err := catalog.CreateDatabase(" shop "); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate database error = %v", err)
	}
	schema, err := catalog.CreateSchema(database.ID, "Sales")
	if err != nil {
		t.Fatalf("create schema: %v", err)
	}
	if _, err := catalog.CreateSchema(database.ID, "sales"); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("duplicate schema error = %v", err)
	}
	orders, err := catalog.CreateTable(schema.ID, "orders", []codec.ColumnDescriptor{{ID: 1, Name: "id", Type: types.BigIntType()}}, []uint32{1})
	if err != nil {
		t.Fatalf("create orders: %v", err)
	}
	if _, err := catalog.CreateTable(schema.ID, "customers", []codec.ColumnDescriptor{{ID: 1, Name: "id", Type: types.BigIntType()}}, []uint32{1}); err != nil {
		t.Fatalf("create customers: %v", err)
	}
	tables, err := catalog.ListTables(schema.ID)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	if len(tables) != 2 || tables[0].Name != "customers" || tables[1].Name != "orders" {
		t.Fatalf("tables are not name-sorted: %+v", tables)
	}
	if err := catalog.DropSchema(schema.ID); !errors.Is(err, ErrDependency) {
		t.Fatalf("drop populated schema error = %v", err)
	}
	if err := catalog.DropDatabase(database.ID); !errors.Is(err, ErrDependency) {
		t.Fatalf("drop populated database error = %v", err)
	}
	for _, table := range tables {
		if err := catalog.DropTable(table.ID); err != nil {
			t.Fatalf("drop table %s: %v", table.Name, err)
		}
	}
	if _, err := catalog.GetTableByID(orders.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("dropped table error = %v", err)
	}
	if err := catalog.DropSchema(schema.ID); err != nil {
		t.Fatalf("drop empty schema: %v", err)
	}
	if err := catalog.DropDatabase(database.ID); err != nil {
		t.Fatalf("drop empty database: %v", err)
	}
	_ = store
}

func TestCatalogForeignKeyAndPartitionValidation(t *testing.T) {
	catalog, _ := openTestCatalog(t)
	_, schema, err := catalog.EnsureDefaults()
	if err != nil {
		t.Fatalf("defaults: %v", err)
	}
	users, err := catalog.CreateTable(schema.ID, "users", []codec.ColumnDescriptor{{ID: 1, Name: "id", Type: types.BigIntType()}}, []uint32{1})
	if err != nil {
		t.Fatalf("users: %v", err)
	}
	orders, err := catalog.CreateTable(schema.ID, "orders", []codec.ColumnDescriptor{
		{ID: 1, Name: "id", Type: types.BigIntType()},
		{ID: 2, Name: "user_id", Type: types.BigIntType()},
	}, []uint32{1})
	if err != nil {
		t.Fatalf("orders: %v", err)
	}
	orders, err = catalog.AddConstraint(orders.ID, ConstraintDescriptor{
		Name:                "orders_user_fk",
		Kind:                ForeignKeyConstraint,
		ColumnIDs:           []uint32{2},
		ReferencedTableID:   users.ID,
		ReferencedColumnIDs: []uint32{1},
	})
	if err != nil || len(orders.Constraints) != 2 {
		t.Fatalf("foreign key: descriptor=%+v err=%v", orders, err)
	}
	if err := catalog.DropTable(users.ID); !errors.Is(err, ErrDependency) {
		t.Fatalf("drop referenced table error = %v", err)
	}
	if _, err := catalog.AddConstraint(orders.ID, ConstraintDescriptor{
		Name: "bad_fk", Kind: ForeignKeyConstraint, ColumnIDs: []uint32{2},
		ReferencedTableID: users.ID, ReferencedColumnIDs: []uint32{99},
	}); err == nil {
		t.Fatal("expected unknown referenced column to fail")
	}
	if _, err := catalog.AddPartition(users.ID, []byte("z"), []byte("a")); err == nil {
		t.Fatal("expected reversed partition to fail")
	}
	users, err = catalog.AddPartition(users.ID, nil, []byte("m"))
	if err != nil {
		t.Fatalf("first partition: %v", err)
	}
	if _, err := catalog.AddPartition(users.ID, []byte("k"), []byte("z")); err == nil {
		t.Fatal("expected overlapping partition to fail")
	}
}

func TestCatalogRejectsCorruptedRecord(t *testing.T) {
	catalog, store := openTestCatalog(t)
	database, err := catalog.CreateDatabase("corrupt_me")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	key := descriptorKey(databaseKind, database.ID)
	value, found, err := store.Get(key)
	if err != nil || !found {
		t.Fatalf("read raw descriptor: found=%v err=%v", found, err)
	}
	value[len(value)-1] ^= 0xff
	if err := store.Put(key, value); err != nil {
		t.Fatalf("write corrupt descriptor: %v", err)
	}
	if _, err := catalog.GetDatabase("corrupt_me"); err == nil {
		t.Fatal("expected descriptor checksum failure")
	}
}

func TestConcurrentCreateHasSingleWinner(t *testing.T) {
	catalog, _ := openTestCatalog(t)
	database, err := catalog.CreateDatabase("concurrent")
	if err != nil {
		t.Fatalf("database: %v", err)
	}
	var successes atomic.Int32
	var wait sync.WaitGroup
	for index := 0; index < 16; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if _, err := catalog.CreateSchema(database.ID, "same_name"); err == nil {
				successes.Add(1)
			} else if !errors.Is(err, ErrAlreadyExists) {
				t.Errorf("unexpected create error: %v", err)
			}
		}()
	}
	wait.Wait()
	if successes.Load() != 1 {
		t.Fatalf("successful creates = %d, want 1", successes.Load())
	}
}

func TestCatalogKeysNeverContainUserNames(t *testing.T) {
	catalog, store := openTestCatalog(t)
	database, err := catalog.CreateDatabase("a_sensitive_name")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	entries, err := store.Scan(catalogKeyPrefix, codec.PrefixEnd(catalogKeyPrefix))
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(entries) != 1 || bytes.Contains(entries[0].Key, []byte(database.Name)) {
		t.Fatalf("catalog storage key leaked user name: %q", entries[0].Key)
	}
}

func openTestCatalog(t *testing.T) (*Catalog, *lsm.Store) {
	t.Helper()
	store, err := lsm.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open LSM: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close LSM: %v", err)
		}
	})
	catalog, err := New(store)
	if err != nil {
		t.Fatalf("new catalog: %v", err)
	}
	return catalog, store
}
