package codec

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"pebbledb/types"
	"sort"
)

var rowMagic = [8]byte{'P', 'D', 'B', 'R', 'O', 'W', '0', '1'}

const (
	rowHeaderSize       = 20
	rowChecksumSize     = 4
	rowColumnHeaderSize = 8
	maxEncodedValueSize = 64 << 20
)

// Row is a schema-versioned set of values keyed by stable catalog column ID.
// Missing IDs are distinct from SQL NULL values.
type Row struct {
	SchemaVersion uint64
	Values        map[uint32]types.Value
}

// EncodeRow produces a deterministic encoding independent of Go map order.
func EncodeRow(row Row) ([]byte, error) {
	if row.SchemaVersion == 0 {
		return nil, fmt.Errorf("codec: row schema version must be positive")
	}
	columnIDs := make([]uint32, 0, len(row.Values))
	for columnID := range row.Values {
		if columnID == 0 {
			return nil, fmt.Errorf("codec: column ID must be positive")
		}
		columnIDs = append(columnIDs, columnID)
	}
	sort.Slice(columnIDs, func(i, j int) bool { return columnIDs[i] < columnIDs[j] })

	result := make([]byte, rowHeaderSize)
	copy(result[:8], rowMagic[:])
	binary.LittleEndian.PutUint64(result[8:16], row.SchemaVersion)
	binary.LittleEndian.PutUint32(result[16:20], uint32(len(columnIDs)))
	for _, columnID := range columnIDs {
		encodedValue, err := encodeValue(row.Values[columnID])
		if err != nil {
			return nil, fmt.Errorf("codec: encode column %d: %w", columnID, err)
		}
		if len(encodedValue) > maxEncodedValueSize {
			return nil, fmt.Errorf("codec: encoded column %d is too large", columnID)
		}
		var header [rowColumnHeaderSize]byte
		binary.LittleEndian.PutUint32(header[:4], columnID)
		binary.LittleEndian.PutUint32(header[4:], uint32(len(encodedValue)))
		result = append(result, header[:]...)
		result = append(result, encodedValue...)
	}
	var checksum [rowChecksumSize]byte
	binary.LittleEndian.PutUint32(checksum[:], crc32.ChecksumIEEE(result))
	return append(result, checksum[:]...), nil
}

// DecodeRow validates and decodes a row produced by EncodeRow.
func DecodeRow(encoded []byte) (Row, error) {
	if len(encoded) < rowHeaderSize+rowChecksumSize {
		return Row{}, fmt.Errorf("codec: row is truncated")
	}
	var magic [8]byte
	copy(magic[:], encoded[:8])
	if magic != rowMagic {
		return Row{}, fmt.Errorf("codec: unsupported row encoding version")
	}
	dataEnd := len(encoded) - rowChecksumSize
	expectedChecksum := binary.LittleEndian.Uint32(encoded[dataEnd:])
	if crc32.ChecksumIEEE(encoded[:dataEnd]) != expectedChecksum {
		return Row{}, fmt.Errorf("codec: row checksum mismatch")
	}

	row := Row{
		SchemaVersion: binary.LittleEndian.Uint64(encoded[8:16]),
		Values:        make(map[uint32]types.Value),
	}
	if row.SchemaVersion == 0 {
		return Row{}, fmt.Errorf("codec: row schema version must be positive")
	}
	columnCount := binary.LittleEndian.Uint32(encoded[16:20])
	if uint64(columnCount) > uint64(dataEnd-rowHeaderSize)/rowColumnHeaderSize {
		return Row{}, fmt.Errorf("codec: impossible row column count")
	}
	offset := rowHeaderSize
	var previousColumnID uint32
	for index := uint32(0); index < columnCount; index++ {
		if dataEnd-offset < rowColumnHeaderSize {
			return Row{}, fmt.Errorf("codec: truncated row column header")
		}
		columnID := binary.LittleEndian.Uint32(encoded[offset : offset+4])
		valueLength := binary.LittleEndian.Uint32(encoded[offset+4 : offset+8])
		offset += rowColumnHeaderSize
		if columnID == 0 || (index > 0 && columnID <= previousColumnID) {
			return Row{}, fmt.Errorf("codec: row column IDs are not strictly ordered")
		}
		if valueLength > maxEncodedValueSize || uint64(valueLength) > uint64(dataEnd-offset) {
			return Row{}, fmt.Errorf("codec: invalid encoded value length for column %d", columnID)
		}
		value, err := decodeValue(encoded[offset : offset+int(valueLength)])
		if err != nil {
			return Row{}, fmt.Errorf("codec: decode column %d: %w", columnID, err)
		}
		row.Values[columnID] = value
		previousColumnID = columnID
		offset += int(valueLength)
	}
	if offset != dataEnd {
		return Row{}, fmt.Errorf("codec: row contains trailing data")
	}
	return row, nil
}
