package lsm

import (
	"encoding/binary"
	"hash/crc32"
	"io"
	"os"
)

var (
	walMagic = [8]byte{'P', 'D', 'B', 'W', 'A', 'L', '0', '1'}
	sstMagic = [8]byte{'P', 'D', 'B', 'S', 'S', 'T', '0', '1'}
)

func checksumEntry(key, value []byte, tombstone bool) uint32 {
	var metadata [8]byte
	binary.LittleEndian.PutUint32(metadata[:4], uint32(len(key)))
	valueLength := int32(len(value))
	if tombstone {
		valueLength = -1
	}
	binary.LittleEndian.PutUint32(metadata[4:], uint32(valueLength))

	checksum := crc32.NewIEEE()
	_, _ = checksum.Write(metadata[:])
	_, _ = checksum.Write(key)
	_, _ = checksum.Write(value)
	return checksum.Sum32()
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
