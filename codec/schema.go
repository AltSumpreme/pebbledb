package codec

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"pebbledb/types"
	"unicode/utf8"
)

var schemaMagic = [8]byte{'P', 'D', 'B', 'S', 'C', 'H', '0', '1'}

const (
	schemaHeaderSize       = 24
	schemaColumnHeaderSize = 12
	maxSchemaNameSize      = 1 << 20
)

type ColumnDescriptor struct {
	ID       uint32
	Name     string
	Type     types.Type
	Nullable bool
}

// TableSchema is the versioned relational shape used to encode and interpret
// rows. Column order is significant and primary-key IDs refer to Columns.
type TableSchema struct {
	Version    uint64
	Columns    []ColumnDescriptor
	PrimaryKey []uint32
}

func EncodeTableSchema(schema TableSchema) ([]byte, error) {
	if err := validateTableSchema(schema); err != nil {
		return nil, err
	}
	result := make([]byte, schemaHeaderSize)
	copy(result[:8], schemaMagic[:])
	binary.LittleEndian.PutUint64(result[8:16], schema.Version)
	binary.LittleEndian.PutUint32(result[16:20], uint32(len(schema.Columns)))
	binary.LittleEndian.PutUint32(result[20:24], uint32(len(schema.PrimaryKey)))
	for _, column := range schema.Columns {
		var header [schemaColumnHeaderSize]byte
		binary.LittleEndian.PutUint32(header[:4], column.ID)
		if column.Nullable {
			header[4] = 1
		}
		header[5] = byte(column.Type.Kind())
		header[6] = byte(column.Type.Precision())
		header[7] = byte(column.Type.Scale())
		binary.LittleEndian.PutUint32(header[8:12], uint32(len(column.Name)))
		result = append(result, header[:]...)
		result = append(result, []byte(column.Name)...)
	}
	for _, columnID := range schema.PrimaryKey {
		var encodedID [4]byte
		binary.LittleEndian.PutUint32(encodedID[:], columnID)
		result = append(result, encodedID[:]...)
	}
	var checksum [4]byte
	binary.LittleEndian.PutUint32(checksum[:], crc32.ChecksumIEEE(result))
	return append(result, checksum[:]...), nil
}

func DecodeTableSchema(encoded []byte) (TableSchema, error) {
	if len(encoded) < schemaHeaderSize+4 {
		return TableSchema{}, fmt.Errorf("codec: table schema is truncated")
	}
	var magic [8]byte
	copy(magic[:], encoded[:8])
	if magic != schemaMagic {
		return TableSchema{}, fmt.Errorf("codec: unsupported table schema encoding version")
	}
	dataEnd := len(encoded) - 4
	if crc32.ChecksumIEEE(encoded[:dataEnd]) != binary.LittleEndian.Uint32(encoded[dataEnd:]) {
		return TableSchema{}, fmt.Errorf("codec: table schema checksum mismatch")
	}
	schema := TableSchema{Version: binary.LittleEndian.Uint64(encoded[8:16])}
	columnCount := binary.LittleEndian.Uint32(encoded[16:20])
	primaryKeyCount := binary.LittleEndian.Uint32(encoded[20:24])
	if uint64(columnCount) > uint64(dataEnd-schemaHeaderSize)/schemaColumnHeaderSize {
		return TableSchema{}, fmt.Errorf("codec: impossible table schema column count")
	}
	offset := schemaHeaderSize
	for index := uint32(0); index < columnCount; index++ {
		if dataEnd-offset < schemaColumnHeaderSize {
			return TableSchema{}, fmt.Errorf("codec: truncated table schema column")
		}
		columnID := binary.LittleEndian.Uint32(encoded[offset : offset+4])
		nullable := encoded[offset+4]
		if nullable > 1 {
			return TableSchema{}, fmt.Errorf("codec: invalid nullable flag")
		}
		typeValue, err := types.NewType(types.Kind(encoded[offset+5]), int(encoded[offset+6]), int(encoded[offset+7]))
		if err != nil {
			return TableSchema{}, err
		}
		nameLength := binary.LittleEndian.Uint32(encoded[offset+8 : offset+12])
		offset += schemaColumnHeaderSize
		if nameLength > maxSchemaNameSize || uint64(nameLength) > uint64(dataEnd-offset) {
			return TableSchema{}, fmt.Errorf("codec: invalid table schema column name length")
		}
		name := string(encoded[offset : offset+int(nameLength)])
		offset += int(nameLength)
		schema.Columns = append(schema.Columns, ColumnDescriptor{
			ID:       columnID,
			Name:     name,
			Type:     typeValue,
			Nullable: nullable == 1,
		})
	}
	if uint64(primaryKeyCount)*4 > uint64(dataEnd-offset) {
		return TableSchema{}, fmt.Errorf("codec: truncated table schema primary key")
	}
	for index := uint32(0); index < primaryKeyCount; index++ {
		schema.PrimaryKey = append(schema.PrimaryKey, binary.LittleEndian.Uint32(encoded[offset:offset+4]))
		offset += 4
	}
	if offset != dataEnd {
		return TableSchema{}, fmt.Errorf("codec: table schema contains trailing data")
	}
	if err := validateTableSchema(schema); err != nil {
		return TableSchema{}, err
	}
	return schema, nil
}

func validateTableSchema(schema TableSchema) error {
	if schema.Version == 0 {
		return fmt.Errorf("codec: table schema version must be positive")
	}
	if len(schema.Columns) == 0 {
		return fmt.Errorf("codec: table schema requires at least one column")
	}
	columnIDs := make(map[uint32]struct{}, len(schema.Columns))
	columnNames := make(map[string]struct{}, len(schema.Columns))
	for _, column := range schema.Columns {
		if column.ID == 0 {
			return fmt.Errorf("codec: column ID must be positive")
		}
		if _, exists := columnIDs[column.ID]; exists {
			return fmt.Errorf("codec: duplicate column ID %d", column.ID)
		}
		columnIDs[column.ID] = struct{}{}
		if column.Name == "" || len(column.Name) > maxSchemaNameSize || !utf8.ValidString(column.Name) {
			return fmt.Errorf("codec: invalid column name")
		}
		if _, exists := columnNames[column.Name]; exists {
			return fmt.Errorf("codec: duplicate column name %q", column.Name)
		}
		columnNames[column.Name] = struct{}{}
		if !column.Type.Valid() {
			return fmt.Errorf("codec: column %q has an invalid type", column.Name)
		}
	}
	primaryKeyIDs := make(map[uint32]struct{}, len(schema.PrimaryKey))
	for _, columnID := range schema.PrimaryKey {
		if _, exists := columnIDs[columnID]; !exists {
			return fmt.Errorf("codec: primary key references unknown column ID %d", columnID)
		}
		if _, exists := primaryKeyIDs[columnID]; exists {
			return fmt.Errorf("codec: duplicate primary-key column ID %d", columnID)
		}
		primaryKeyIDs[columnID] = struct{}{}
	}
	return nil
}
