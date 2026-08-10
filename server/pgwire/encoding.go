package pgwire

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"

	"pebbledb/sql/executor"
	"pebbledb/types"
)

const (
	boolOID    uint32 = 16
	int8OID    uint32 = 20
	int4OID    uint32 = 23
	textOID    uint32 = 25
	numericOID uint32 = 1700
)

func rowDescription(columns []executor.ResultColumn, formats []int16) []byte {
	var payload bytes.Buffer
	_ = binary.Write(&payload, binary.BigEndian, int16(len(columns)))
	for index, column := range columns {
		payload.Write(cstring(column.Name))
		_ = binary.Write(&payload, binary.BigEndian, uint32(0))
		_ = binary.Write(&payload, binary.BigEndian, int16(0))
		oid, size, modifier := pgType(column.Type)
		_ = binary.Write(&payload, binary.BigEndian, oid)
		_ = binary.Write(&payload, binary.BigEndian, size)
		_ = binary.Write(&payload, binary.BigEndian, modifier)
		format := int16(0)
		if index < len(formats) {
			format = formats[index]
		}
		_ = binary.Write(&payload, binary.BigEndian, format)
	}
	return payload.Bytes()
}

func dataRow(values []types.Value, formats []int16) ([]byte, error) {
	var payload bytes.Buffer
	_ = binary.Write(&payload, binary.BigEndian, int16(len(values)))
	for index, value := range values {
		if value.IsNull() {
			_ = binary.Write(&payload, binary.BigEndian, int32(-1))
			continue
		}
		format := int16(0)
		if index < len(formats) {
			format = formats[index]
		}
		encoded, err := encodeValue(value, format)
		if err != nil {
			return nil, err
		}
		_ = binary.Write(&payload, binary.BigEndian, int32(len(encoded)))
		payload.Write(encoded)
	}
	return payload.Bytes(), nil
}

func encodeValue(value types.Value, format int16) ([]byte, error) {
	if format == 0 {
		if value.Type().Kind() == types.BoolKind {
			boolean, _ := value.AsBool()
			if boolean {
				return []byte("t"), nil
			}
			return []byte("f"), nil
		}
		return []byte(value.String()), nil
	}
	if format != 1 {
		return nil, fmt.Errorf("pgwire: unknown result format %d", format)
	}
	switch value.Type().Kind() {
	case types.BoolKind:
		boolean, _ := value.AsBool()
		if boolean {
			return []byte{1}, nil
		}
		return []byte{0}, nil
	case types.IntKind:
		integer, _ := value.AsInt64()
		var result [4]byte
		binary.BigEndian.PutUint32(result[:], uint32(int32(integer)))
		return result[:], nil
	case types.BigIntKind:
		integer, _ := value.AsInt64()
		var result [8]byte
		binary.BigEndian.PutUint64(result[:], uint64(integer))
		return result[:], nil
	case types.TextKind:
		text, _ := value.AsText()
		return []byte(text), nil
	case types.DecimalKind:
		return encodeNumeric(value)
	default:
		return nil, fmt.Errorf("pgwire: cannot encode %s", value.Type())
	}
}

func encodeNumeric(value types.Value) ([]byte, error) {
	raw := value.String()
	sign := int16(0)
	if strings.HasPrefix(raw, "-") {
		sign, raw = 0x4000, raw[1:]
	}
	parts := strings.SplitN(raw, ".", 2)
	integer, fractional := parts[0], ""
	if len(parts) == 2 {
		fractional = parts[1]
	}
	dscale := len(fractional)
	integer = strings.Repeat("0", (4-len(integer)%4)%4) + integer
	integerGroups := len(integer) / 4
	fractional += strings.Repeat("0", (4-len(fractional)%4)%4)
	groups := make([]int16, 0, integerGroups+len(fractional)/4)
	for offset := 0; offset < len(integer); offset += 4 {
		value, _ := strconv.Atoi(integer[offset : offset+4])
		groups = append(groups, int16(value))
	}
	for offset := 0; offset < len(fractional); offset += 4 {
		value, _ := strconv.Atoi(fractional[offset : offset+4])
		groups = append(groups, int16(value))
	}
	leading := 0
	for leading < len(groups) && groups[leading] == 0 {
		leading++
	}
	groups = groups[leading:]
	weight := int16(integerGroups - 1 - leading)
	if len(groups) == 0 {
		weight = 0
	}
	var result bytes.Buffer
	_ = binary.Write(&result, binary.BigEndian, int16(len(groups)))
	_ = binary.Write(&result, binary.BigEndian, weight)
	_ = binary.Write(&result, binary.BigEndian, sign)
	_ = binary.Write(&result, binary.BigEndian, int16(dscale))
	for _, group := range groups {
		_ = binary.Write(&result, binary.BigEndian, group)
	}
	return result.Bytes(), nil
}

func pgType(value types.Type) (uint32, int16, int32) {
	switch value.Kind() {
	case types.BoolKind:
		return boolOID, 1, -1
	case types.IntKind:
		return int4OID, 4, -1
	case types.BigIntKind:
		return int8OID, 8, -1
	case types.DecimalKind:
		modifier := int32((value.Precision()<<16)|value.Scale()) + 4
		return numericOID, -1, modifier
	default:
		return textOID, -1, -1
	}
}

func normalizeFormats(formats []int16, count int) ([]int16, error) {
	if len(formats) == 0 {
		return make([]int16, count), nil
	}
	if len(formats) == 1 {
		result := make([]int16, count)
		for index := range result {
			result[index] = formats[0]
		}
		return result, validateFormats(result)
	}
	if len(formats) != count {
		return nil, fmt.Errorf("pgwire: got %d format codes for %d values", len(formats), count)
	}
	result := append([]int16(nil), formats...)
	return result, validateFormats(result)
}

func validateFormats(formats []int16) error {
	for _, format := range formats {
		if format != 0 && format != 1 {
			return fmt.Errorf("pgwire: unsupported format code %d", format)
		}
	}
	return nil
}
