package indexstore_test

import (
	"errors"
	"testing"

	"pebbledb/catalog"
	"pebbledb/codec"
	"pebbledb/storage/indexstore"
	"pebbledb/storage/lsm"
	"pebbledb/types"
)

func TestUniqueAndNonUniqueIndexes(t *testing.T) {
	lsmStore, err := lsm.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = lsmStore.Close() })
	store, _ := indexstore.New(lsmStore)
	table := indexedTable()
	rows := []codec.Row{indexedRow(1, "a@example.com", "IN"), indexedRow(2, "b@example.com", "IN"), indexedRow(3, "c@example.com", "US")}
	if err := store.CheckBatch(table, rows); err != nil {
		t.Fatalf("check batch: %v", err)
	}
	for _, row := range rows {
		if err := store.Add(table, row); err != nil {
			t.Fatalf("add: %v", err)
		}
	}
	if err := store.Check(table, indexedRow(4, "a@example.com", "GB"), nil); !errors.Is(err, indexstore.ErrUniqueViolation) {
		t.Fatalf("unique error = %v", err)
	}
	country, _ := types.TextValue("IN")
	primaryKeys, err := store.Lookup(table, table.Indexes[1], []types.Value{country})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if len(primaryKeys) != 2 || primaryKeys[0][0].String() != "1" || primaryKeys[1][0].String() != "2" {
		t.Fatalf("unexpected lookup keys: %+v", primaryKeys)
	}
	entries, distinct, err := store.IndexStats(table, table.Indexes[1])
	if err != nil || entries != 3 || distinct != 2 {
		t.Fatalf("stats = (%d, %d, %v)", entries, distinct, err)
	}
	if err := store.Remove(table, rows[0]); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := store.Check(table, indexedRow(4, "a@example.com", "GB"), nil); err != nil {
		t.Fatalf("unique value was not released: %v", err)
	}
}

func TestBatchAndBackfillRejectDuplicatesWithoutEntries(t *testing.T) {
	lsmStore, err := lsm.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = lsmStore.Close() })
	store, _ := indexstore.New(lsmStore)
	table := indexedTable()
	duplicateRows := []codec.Row{indexedRow(1, "same@example.com", "IN"), indexedRow(2, "same@example.com", "US")}
	if err := store.CheckBatch(table, duplicateRows); !errors.Is(err, indexstore.ErrUniqueViolation) {
		t.Fatalf("batch unique error = %v", err)
	}
	if err := store.Backfill(table, table.Indexes[0], duplicateRows); !errors.Is(err, indexstore.ErrUniqueViolation) {
		t.Fatalf("backfill unique error = %v", err)
	}
	entries, _, err := store.IndexStats(table, table.Indexes[0])
	if err != nil || entries != 0 {
		t.Fatalf("failed backfill left %d entries: %v", entries, err)
	}
}

func indexedTable() catalog.TableDescriptor {
	return catalog.TableDescriptor{
		ID: 10, SchemaID: 1, Name: "users", Version: 1,
		Schema: codec.TableSchema{Version: 1, Columns: []codec.ColumnDescriptor{
			{ID: 1, Name: "id", Type: types.BigIntType()},
			{ID: 2, Name: "email", Type: types.TextType()},
			{ID: 3, Name: "country", Type: types.TextType()},
		}, PrimaryKey: []uint32{1}},
		Indexes: []catalog.IndexDescriptor{
			{ID: 20, Name: "users_email_key", ColumnIDs: []uint32{2}, Unique: true},
			{ID: 21, Name: "users_country_idx", ColumnIDs: []uint32{3}},
		},
	}
}

func indexedRow(id int64, email, country string) codec.Row {
	emailValue, _ := types.TextValue(email)
	countryValue, _ := types.TextValue(country)
	return codec.Row{SchemaVersion: 1, Values: map[uint32]types.Value{
		1: types.BigIntValue(id), 2: emailValue, 3: countryValue,
	}}
}
