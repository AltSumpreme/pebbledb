package codec

import (
	"encoding/binary"
	"fmt"
	"pebbledb/types"
)

func encodeValue(value types.Value) ([]byte, error) {
	if !value.Valid() {
		return nil, fmt.Errorf("codec: cannot encode invalid SQL value")
	}
	typeValue := value.Type()
	result := []byte{byte(typeValue.Kind()), byte(typeValue.Precision()), byte(typeValue.Scale()), 0}
	if value.IsNull() {
		return result, nil
	}
	result[3] = 1
	switch typeValue.Kind() {
	case types.BoolKind:
		boolean, _ := value.AsBool()
		if boolean {
			return append(result, 1), nil
		}
		return append(result, 0), nil
	case types.IntKind:
		integer, _ := value.AsInt64()
		var raw [4]byte
		binary.LittleEndian.PutUint32(raw[:], uint32(int32(integer)))
		return append(result, raw[:]...), nil
	case types.BigIntKind:
		integer, _ := value.AsInt64()
		var raw [8]byte
		binary.LittleEndian.PutUint64(raw[:], uint64(integer))
		return append(result, raw[:]...), nil
	case types.TextKind:
		text, _ := value.AsText()
		return append(result, []byte(text)...), nil
	case types.DecimalKind:
		coefficient, _ := value.DecimalCoefficient()
		raw, err := encodeInt128(coefficient)
		if err != nil {
			return nil, err
		}
		return append(result, raw[:]...), nil
	default:
		return nil, fmt.Errorf("codec: unsupported value type %s", typeValue)
	}
}

func decodeValue(encoded []byte) (types.Value, error) {
	if len(encoded) < 4 {
		return types.Value{}, fmt.Errorf("codec: truncated value descriptor")
	}
	typeValue, err := types.NewType(types.Kind(encoded[0]), int(encoded[1]), int(encoded[2]))
	if err != nil {
		return types.Value{}, err
	}
	if encoded[3] == 0 {
		if len(encoded) != 4 {
			return types.Value{}, fmt.Errorf("codec: NULL value contains a payload")
		}
		return types.NullValue(typeValue)
	}
	if encoded[3] != 1 {
		return types.Value{}, fmt.Errorf("codec: invalid NULL marker %d", encoded[3])
	}
	payload := encoded[4:]
	switch typeValue.Kind() {
	case types.BoolKind:
		if len(payload) != 1 || payload[0] > 1 {
			return types.Value{}, fmt.Errorf("codec: invalid BOOL payload")
		}
		return types.BoolValue(payload[0] == 1), nil
	case types.IntKind:
		if len(payload) != 4 {
			return types.Value{}, fmt.Errorf("codec: invalid INT payload length")
		}
		return types.IntValue(int32(binary.LittleEndian.Uint32(payload))), nil
	case types.BigIntKind:
		if len(payload) != 8 {
			return types.Value{}, fmt.Errorf("codec: invalid BIGINT payload length")
		}
		return types.BigIntValue(int64(binary.LittleEndian.Uint64(payload))), nil
	case types.TextKind:
		return types.TextValue(string(payload))
	case types.DecimalKind:
		coefficient, err := decodeInt128(payload)
		if err != nil {
			return types.Value{}, err
		}
		return types.DecimalValueFromCoefficient(typeValue, coefficient)
	default:
		return types.Value{}, fmt.Errorf("codec: unsupported value type %s", typeValue)
	}
}
