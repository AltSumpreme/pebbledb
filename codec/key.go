// Package codec contains versioned persistent encodings for relational data.
package codec

import (
	"encoding/binary"
	"fmt"
	"pebbledb/types"
)

const keyEncodingVersion byte = 1

// EncodeKey encodes a composite SQL key. For values of the same declared types,
// bytewise comparison of the result is identical to types.Compare ordering.
func EncodeKey(values ...types.Value) ([]byte, error) {
	encoded := []byte{keyEncodingVersion}
	for _, value := range values {
		var err error
		encoded, err = appendKeyValue(encoded, value)
		if err != nil {
			return nil, err
		}
	}
	return encoded, nil
}

func appendKeyValue(destination []byte, value types.Value) ([]byte, error) {
	if !value.Valid() {
		return nil, fmt.Errorf("codec: cannot encode invalid SQL value")
	}
	typeValue := value.Type()
	destination = append(destination, byte(typeValue.Kind()), byte(typeValue.Precision()), byte(typeValue.Scale()))
	if value.IsNull() {
		return append(destination, 0), nil
	}
	destination = append(destination, 1)

	switch typeValue.Kind() {
	case types.BoolKind:
		boolean, _ := value.AsBool()
		if boolean {
			return append(destination, 1), nil
		}
		return append(destination, 0), nil
	case types.IntKind:
		integer, _ := value.AsInt64()
		var raw [4]byte
		binary.BigEndian.PutUint32(raw[:], uint32(int32(integer))^0x80000000)
		return append(destination, raw[:]...), nil
	case types.BigIntKind:
		integer, _ := value.AsInt64()
		var raw [8]byte
		binary.BigEndian.PutUint64(raw[:], uint64(integer)^0x8000000000000000)
		return append(destination, raw[:]...), nil
	case types.TextKind:
		text, _ := value.AsText()
		for _, current := range []byte(text) {
			if current == 0 {
				destination = append(destination, 0, 0xff)
			} else {
				destination = append(destination, current)
			}
		}
		return append(destination, 0, 0), nil
	case types.DecimalKind:
		coefficient, _ := value.DecimalCoefficient()
		raw, err := encodeInt128(coefficient)
		if err != nil {
			return nil, err
		}
		raw[0] ^= 0x80
		return append(destination, raw[:]...), nil
	default:
		return nil, fmt.Errorf("codec: unsupported key type %s", typeValue)
	}
}

// DecodeKey decodes a key produced by EncodeKey and rejects unknown versions or
// non-canonical component encodings.
func DecodeKey(encoded []byte) ([]types.Value, error) {
	if len(encoded) == 0 || encoded[0] != keyEncodingVersion {
		return nil, fmt.Errorf("codec: unsupported key encoding version")
	}
	var result []types.Value
	for offset := 1; offset < len(encoded); {
		if len(encoded)-offset < 4 {
			return nil, fmt.Errorf("codec: truncated key component")
		}
		typeValue, err := types.NewType(types.Kind(encoded[offset]), int(encoded[offset+1]), int(encoded[offset+2]))
		if err != nil {
			return nil, err
		}
		nullMarker := encoded[offset+3]
		offset += 4
		if nullMarker == 0 {
			null, _ := types.NullValue(typeValue)
			result = append(result, null)
			continue
		}
		if nullMarker != 1 {
			return nil, fmt.Errorf("codec: invalid NULL marker %d", nullMarker)
		}

		var value types.Value
		switch typeValue.Kind() {
		case types.BoolKind:
			if len(encoded)-offset < 1 || encoded[offset] > 1 {
				return nil, fmt.Errorf("codec: invalid BOOL key component")
			}
			value = types.BoolValue(encoded[offset] == 1)
			offset++
		case types.IntKind:
			if len(encoded)-offset < 4 {
				return nil, fmt.Errorf("codec: truncated INT key component")
			}
			raw := binary.BigEndian.Uint32(encoded[offset:offset+4]) ^ 0x80000000
			value = types.IntValue(int32(raw))
			offset += 4
		case types.BigIntKind:
			if len(encoded)-offset < 8 {
				return nil, fmt.Errorf("codec: truncated BIGINT key component")
			}
			raw := binary.BigEndian.Uint64(encoded[offset:offset+8]) ^ 0x8000000000000000
			value = types.BigIntValue(int64(raw))
			offset += 8
		case types.TextKind:
			decoded, next, err := decodeEscapedText(encoded, offset)
			if err != nil {
				return nil, err
			}
			value, err = types.TextValue(string(decoded))
			if err != nil {
				return nil, err
			}
			offset = next
		case types.DecimalKind:
			if len(encoded)-offset < 16 {
				return nil, fmt.Errorf("codec: truncated DECIMAL key component")
			}
			raw := append([]byte(nil), encoded[offset:offset+16]...)
			raw[0] ^= 0x80
			coefficient, err := decodeInt128(raw)
			if err != nil {
				return nil, err
			}
			value, err = types.DecimalValueFromCoefficient(typeValue, coefficient)
			if err != nil {
				return nil, err
			}
			offset += 16
		default:
			return nil, fmt.Errorf("codec: unsupported key component type %s", typeValue)
		}
		result = append(result, value)
	}
	return result, nil
}

func decodeEscapedText(encoded []byte, offset int) ([]byte, int, error) {
	decoded := make([]byte, 0)
	for offset < len(encoded) {
		current := encoded[offset]
		offset++
		if current != 0 {
			decoded = append(decoded, current)
			continue
		}
		if offset >= len(encoded) {
			return nil, 0, fmt.Errorf("codec: truncated TEXT escape")
		}
		escape := encoded[offset]
		offset++
		switch escape {
		case 0:
			return decoded, offset, nil
		case 0xff:
			decoded = append(decoded, 0)
		default:
			return nil, 0, fmt.Errorf("codec: invalid TEXT escape")
		}
	}
	return nil, 0, fmt.Errorf("codec: unterminated TEXT key component")
}

// PrefixEnd returns the exclusive upper bound containing all keys with prefix.
// It returns nil when no finite upper bound exists.
func PrefixEnd(prefix []byte) []byte {
	end := append([]byte(nil), prefix...)
	for index := len(end) - 1; index >= 0; index-- {
		if end[index] != 0xff {
			end[index]++
			return end[:index+1]
		}
	}
	return nil
}
