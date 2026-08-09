package types_test

import (
	"encoding/json"
	"math"
	"testing"

	"pebbledb/types"
)

func TestParseTypeAndTextSerialization(t *testing.T) {
	tests := map[string]string{
		"boolean":        "BOOL",
		"INT4":           "INT",
		" bigint ":       "BIGINT",
		"string":         "TEXT",
		"numeric(18, 4)": "DECIMAL(18,4)",
	}
	for input, expected := range tests {
		parsed, err := types.ParseType(input)
		if err != nil {
			t.Fatalf("parse %q: %v", input, err)
		}
		if parsed.String() != expected {
			t.Fatalf("parse %q = %s, want %s", input, parsed, expected)
		}
		encoded, err := json.Marshal(parsed)
		if err != nil {
			t.Fatalf("marshal %q: %v", input, err)
		}
		var decoded types.Type
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatalf("unmarshal %q: %v", input, err)
		}
		if decoded != parsed {
			t.Fatalf("type JSON round trip = %s, want %s", decoded, parsed)
		}
	}
}

func TestTypeValidation(t *testing.T) {
	invalid := []string{"DECIMAL", "DECIMAL(0,0)", "DECIMAL(39,0)", "DECIMAL(3,4)", "VARCHAR"}
	for _, input := range invalid {
		if _, err := types.ParseType(input); err == nil {
			t.Fatalf("expected %q to be rejected", input)
		}
	}
}

func TestDecimalIsExactAndScaleAware(t *testing.T) {
	decimalType, err := types.DecimalType(10, 3)
	if err != nil {
		t.Fatalf("decimal type: %v", err)
	}
	tests := map[string]string{
		"12.34":   "12.340",
		"-0.001":  "-0.001",
		"+0007.1": "7.100",
		"0":       "0.000",
		"9999999": "9999999.000",
	}
	for input, expected := range tests {
		value, err := types.DecimalValue(decimalType, input)
		if err != nil {
			t.Fatalf("decimal %q: %v", input, err)
		}
		if value.String() != expected {
			t.Fatalf("decimal %q = %s, want %s", input, value, expected)
		}
	}
	if _, err := types.DecimalValue(decimalType, "1.2345"); err == nil {
		t.Fatal("expected scale overflow")
	}
	if _, err := types.DecimalValue(decimalType, "10000000"); err == nil {
		t.Fatal("expected precision overflow")
	}
}

func TestValueComparison(t *testing.T) {
	nullInt, err := types.NullValue(types.IntType())
	if err != nil {
		t.Fatalf("NULL: %v", err)
	}
	ordered := []types.Value{
		nullInt,
		types.IntValue(math.MinInt32),
		types.IntValue(-1),
		types.IntValue(0),
		types.IntValue(math.MaxInt32),
	}
	for index := 1; index < len(ordered); index++ {
		comparison, err := types.Compare(ordered[index-1], ordered[index])
		if err != nil {
			t.Fatalf("compare %d: %v", index, err)
		}
		if comparison >= 0 {
			t.Fatalf("%v did not sort before %v", ordered[index-1], ordered[index])
		}
	}
	if _, err := types.Compare(types.IntValue(1), types.BigIntValue(1)); err == nil {
		t.Fatal("expected comparison across declared types to fail")
	}
}
