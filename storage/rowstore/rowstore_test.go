package rowstore_test

import (
	"errors"
	"testing"

	"pebbledb/catalog"
	"pebbledb/codec"
	"pebbledb/storage/lsm"
	"pebbledb/storage/rowstore"
	"pebbledb/types"
)

func TestRowsPersistAndScanInPrimaryKeyOrder(t *testing.T) {
	directory := t.TempDir()
	lsmStore, err := lsm.Open(directory)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	store, _ := rowstore.New(lsmStore)
	table := testTable()
	for _, id := range []int64{3, 1, 2} {
		if err := store.Insert(table, testRow(id)); err != nil {
			t.Fatalf("insert %d: %v", id, err)
		}
	}
	if err := store.Insert(table, testRow(2)); !errors.Is(err, rowstore.ErrDuplicateKey) {
		t.Fatalf("duplicate error = %v", err)
	}
	if err := lsmStore.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	lsmStore, err = lsm.Open(directory)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = lsmStore.Close() })
	store, _ = rowstore.New(lsmStore)
	rows, err := store.Scan(table)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows", len(rows))
	}
	for index, stored := range rows {
		id, _ := stored.Row.Values[1].AsInt64()
		if id != int64(index+1) {
			t.Fatalf("row %d has primary key %d", index, id)
		}
	}
	row, err := store.Get(table, []types.Value{types.BigIntValue(2)})
	if err != nil || row.Values[2].String() != "user-2" {
		t.Fatalf("get = %+v, %v", row, err)
	}
}

func TestReplaceDeleteAndValidation(t *testing.T) {
	lsmStore, err := lsm.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = lsmStore.Close() })
	store, _ := rowstore.New(lsmStore)
	table := testTable()
	row := testRow(1)
	if err := store.Insert(table, row); err != nil {
		t.Fatalf("insert: %v", err)
	}
	name, _ := types.TextValue("updated")
	row.Values[2] = name
	if err := store.Replace(table, row); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if err := store.Delete(table, row); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := store.Get(table, []types.Value{types.BigIntValue(1)}); !errors.Is(err, rowstore.ErrRowNotFound) {
		t.Fatalf("get deleted error = %v", err)
	}
	invalid := testRow(2)
	invalid.Values[1] = types.IntValue(2)
	if err := store.Insert(table, invalid); err == nil {
		t.Fatal("expected wrong primary-key type to fail")
	}
}

func testTable() catalog.TableDescriptor {
	return catalog.TableDescriptor{
		ID: 10, SchemaID: 5, Name: "users", Version: 1,
		Schema: codec.TableSchema{
			Version: 1,
			Columns: []codec.ColumnDescriptor{
				{ID: 1, Name: "id", Type: types.BigIntType()},
				{ID: 2, Name: "name", Type: types.TextType()},
			},
			PrimaryKey: []uint32{1},
		},
	}
}

func testRow(id int64) codec.Row {
	name, _ := types.TextValue("user-" + types.BigIntValue(id).String())
	return codec.Row{SchemaVersion: 1, Values: map[uint32]types.Value{1: types.BigIntValue(id), 2: name}}
}
