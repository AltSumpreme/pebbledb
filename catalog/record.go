package catalog

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
)

type descriptorKind byte

const (
	databaseKind     descriptorKind = 1
	schemaKind       descriptorKind = 2
	tableKind        descriptorKind = 3
	maxRecordPayload                = 16 << 20
)

var recordMagic = [8]byte{'P', 'D', 'B', 'C', 'A', 'T', '0', '1'}

func encodeRecord(kind descriptorKind, descriptor any) ([]byte, error) {
	payload, err := json.Marshal(descriptor)
	if err != nil {
		return nil, fmt.Errorf("catalog: encode descriptor: %w", err)
	}
	if len(payload) > maxRecordPayload {
		return nil, fmt.Errorf("catalog: descriptor is too large")
	}
	result := make([]byte, 13, 13+len(payload)+4)
	copy(result[:8], recordMagic[:])
	result[8] = byte(kind)
	binary.LittleEndian.PutUint32(result[9:13], uint32(len(payload)))
	result = append(result, payload...)
	var checksum [4]byte
	binary.LittleEndian.PutUint32(checksum[:], crc32.ChecksumIEEE(result))
	return append(result, checksum[:]...), nil
}

func decodeRecord(encoded []byte, expectedKind descriptorKind, destination any) error {
	if len(encoded) < 17 {
		return fmt.Errorf("catalog: descriptor record is truncated")
	}
	var magic [8]byte
	copy(magic[:], encoded[:8])
	if magic != recordMagic || descriptorKind(encoded[8]) != expectedKind {
		return fmt.Errorf("catalog: descriptor record has an unsupported format or kind")
	}
	payloadLength := binary.LittleEndian.Uint32(encoded[9:13])
	if payloadLength > maxRecordPayload || int(payloadLength)+17 != len(encoded) {
		return fmt.Errorf("catalog: descriptor record has an invalid payload length")
	}
	checksumOffset := len(encoded) - 4
	if crc32.ChecksumIEEE(encoded[:checksumOffset]) != binary.LittleEndian.Uint32(encoded[checksumOffset:]) {
		return fmt.Errorf("catalog: descriptor checksum mismatch")
	}
	if err := json.Unmarshal(encoded[13:checksumOffset], destination); err != nil {
		return fmt.Errorf("catalog: decode descriptor: %w", err)
	}
	return nil
}
