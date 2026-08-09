package codec_test

import (
	"bytes"
	"encoding/hex"
	"reflect"
	"testing"

	"pebbledb/codec"
	"pebbledb/storage/lsm"
	"pebbledb/types"
)

func TestKeyGoldenFormats(t *testing.T) {
	encoded, err := codec.EncodeKey(types.IntValue(-1))
	if err != nil {
		t.Fatalf("encode INT: %v", err)
	}
	if got := hex.EncodeToString(encoded); got != "01020000017fffffff" {
		t.Fatalf("INT key format changed: %s", got)
	}
	text, _ := types.TextValue("a\x00b")
	encoded, err = codec.EncodeKey(text)
	if err != nil {
		t.Fatalf("encode TEXT: %v", err)
	}
	if got := hex.EncodeToString(encoded); got != "01040000016100ff620000" {
		t.Fatalf("TEXT key format changed: %s", got)
	}
}

func TestKeyRoundTrip(t *testing.T) {
	decimalType, _ := types.DecimalType(20, 4)
	decimal, _ := types.DecimalValue(decimalType, "-123456789.0123")
	nullText, _ := types.NullValue(types.TextType())
	text, _ := types.TextValue("hello\x00world")
	values := []types.Value{
		types.BoolValue(true),
		types.IntValue(-42),
		types.BigIntValue(1 << 50),
		text,
		decimal,
		nullText,
	}
	encoded, err := codec.EncodeKey(values...)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, err := codec.DecodeKey(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded) != len(values) {
		t.Fatalf("decoded %d values, want %d", len(decoded), len(values))
	}
	for index := range values {
		if !types.Equal(decoded[index], values[index]) {
			t.Fatalf("value %d = %s (%s), want %s (%s)", index, decoded[index], decoded[index].Type(), values[index], values[index].Type())
		}
	}
}

func TestKeyEncodingPreservesOrder(t *testing.T) {
	assertKeyOrder(t, []types.Value{
		types.IntValue(-1 << 31), types.IntValue(-100), types.IntValue(-1),
		types.IntValue(0), types.IntValue(1), types.IntValue(1<<31 - 1),
	})
	var texts []types.Value
	for _, raw := range []string{"", "\x00", "\x00a", "a", "aa", "b", "é"} {
		value, _ := types.TextValue(raw)
		texts = append(texts, value)
	}
	assertKeyOrder(t, texts)
	decimalType, _ := types.DecimalType(8, 2)
	var decimals []types.Value
	for _, raw := range []string{"-999.00", "-1.01", "-1.00", "0", "0.01", "999.00"} {
		value, _ := types.DecimalValue(decimalType, raw)
		decimals = append(decimals, value)
	}
	assertKeyOrder(t, decimals)
}

func TestCompositeKeyPrefixRange(t *testing.T) {
	table, _ := types.TextValue("users")
	prefix, err := codec.EncodeKey(table)
	if err != nil {
		t.Fatalf("prefix: %v", err)
	}
	full, err := codec.EncodeKey(table, types.BigIntValue(42))
	if err != nil {
		t.Fatalf("full: %v", err)
	}
	if !bytes.HasPrefix(full, prefix) {
		t.Fatalf("composite key does not retain encoded prefix")
	}
	end := codec.PrefixEnd(prefix)
	if bytes.Compare(full, prefix) < 0 || bytes.Compare(full, end) >= 0 {
		t.Fatalf("full key is outside prefix range")
	}
}

func TestRowRoundTripDeterminismAndCorruption(t *testing.T) {
	name, _ := types.TextValue("Reuben")
	nullText, _ := types.NullValue(types.TextType())
	rowA := codec.Row{SchemaVersion: 7, Values: map[uint32]types.Value{
		3: nullText,
		1: types.BigIntValue(984),
		2: name,
	}}
	rowB := codec.Row{SchemaVersion: 7, Values: map[uint32]types.Value{
		2: name,
		1: types.BigIntValue(984),
		3: nullText,
	}}
	encodedA, err := codec.EncodeRow(rowA)
	if err != nil {
		t.Fatalf("encode A: %v", err)
	}
	encodedB, err := codec.EncodeRow(rowB)
	if err != nil {
		t.Fatalf("encode B: %v", err)
	}
	if !bytes.Equal(encodedA, encodedB) {
		t.Fatal("row encoding depends on Go map iteration order")
	}
	decoded, err := codec.DecodeRow(encodedA)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.SchemaVersion != rowA.SchemaVersion || len(decoded.Values) != len(rowA.Values) {
		t.Fatalf("unexpected decoded row: %+v", decoded)
	}
	for columnID, expected := range rowA.Values {
		if !types.Equal(decoded.Values[columnID], expected) {
			t.Fatalf("column %d = %v, want %v", columnID, decoded.Values[columnID], expected)
		}
	}
	corrupted := append([]byte(nil), encodedA...)
	corrupted[len(corrupted)/2] ^= 0xff
	if _, err := codec.DecodeRow(corrupted); err == nil {
		t.Fatal("expected corrupted row to be rejected")
	}
}

func TestTableSchemaRoundTripAndValidation(t *testing.T) {
	priceType, _ := types.DecimalType(12, 2)
	schema := codec.TableSchema{
		Version: 3,
		Columns: []codec.ColumnDescriptor{
			{ID: 1, Name: "id", Type: types.BigIntType()},
			{ID: 2, Name: "name", Type: types.TextType(), Nullable: true},
			{ID: 3, Name: "price", Type: priceType},
		},
		PrimaryKey: []uint32{1},
	}
	encoded, err := codec.EncodeTableSchema(schema)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, err := codec.DecodeTableSchema(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !reflect.DeepEqual(decoded, schema) {
		t.Fatalf("decoded schema = %+v, want %+v", decoded, schema)
	}

	invalid := schema
	invalid.PrimaryKey = []uint32{99}
	if _, err := codec.EncodeTableSchema(invalid); err == nil {
		t.Fatal("expected unknown primary-key column to fail")
	}
	corrupted := append([]byte(nil), encoded...)
	corrupted[len(corrupted)-1] ^= 0xff
	if _, err := codec.DecodeTableSchema(corrupted); err == nil {
		t.Fatal("expected corrupted schema to be rejected")
	}
}

func TestTypedRowsRoundTripThroughLSMRange(t *testing.T) {
	store, err := lsm.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open LSM: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	namespace, _ := types.TextValue("table-primary")
	tableID := types.BigIntValue(7)
	prefix, err := codec.EncodeKey(namespace, tableID)
	if err != nil {
		t.Fatalf("encode table prefix: %v", err)
	}
	for id, rawName := range map[int64]string{2: "Bob", 1: "Alice", 3: "Chandra"} {
		name, _ := types.TextValue(rawName)
		key, err := codec.EncodeKey(namespace, tableID, types.BigIntValue(id))
		if err != nil {
			t.Fatalf("encode row key: %v", err)
		}
		row, err := codec.EncodeRow(codec.Row{SchemaVersion: 1, Values: map[uint32]types.Value{
			1: types.BigIntValue(id),
			2: name,
		}})
		if err != nil {
			t.Fatalf("encode row: %v", err)
		}
		if err := store.Put(key, row); err != nil {
			t.Fatalf("put row: %v", err)
		}
	}

	entries, err := store.Scan(prefix, codec.PrefixEnd(prefix))
	if err != nil {
		t.Fatalf("scan table: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("got %d rows, want 3", len(entries))
	}
	for index, entry := range entries {
		keyValues, err := codec.DecodeKey(entry.Key)
		if err != nil {
			t.Fatalf("decode key %d: %v", index, err)
		}
		primaryKey, _ := keyValues[2].AsInt64()
		if primaryKey != int64(index+1) {
			t.Fatalf("row %d primary key = %d", index, primaryKey)
		}
		row, err := codec.DecodeRow(entry.Value)
		if err != nil {
			t.Fatalf("decode row %d: %v", index, err)
		}
		rowID, _ := row.Values[1].AsInt64()
		if rowID != primaryKey {
			t.Fatalf("key ID %d does not match row ID %d", primaryKey, rowID)
		}
	}
}

func assertKeyOrder(t *testing.T, ordered []types.Value) {
	t.Helper()
	var previous []byte
	for index, value := range ordered {
		encoded, err := codec.EncodeKey(value)
		if err != nil {
			t.Fatalf("encode %s: %v", value, err)
		}
		if index > 0 && bytes.Compare(previous, encoded) >= 0 {
			t.Fatalf("encoded %s does not sort after %s", value, ordered[index-1])
		}
		previous = encoded
	}
}
