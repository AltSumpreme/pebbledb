package lsm

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
)

const (
	walPut              byte = 1
	walDelete           byte = 2
	walHeaderSize            = 8
	walRecordHeaderSize      = 8
)

type writeAheadLog struct {
	file *os.File
	path string
}

func openWriteAheadLog(path string, apply func(string, mutation)) (*writeAheadLog, error) {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return nil, fmt.Errorf("open WAL: %w", err)
	}
	w := &writeAheadLog{file: file, path: path}
	if err := w.initializeAndReplay(apply); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("sync WAL directory: %w", err)
	}
	return w, nil
}

func (w *writeAheadLog) initializeAndReplay(apply func(string, mutation)) error {
	info, err := w.file.Stat()
	if err != nil {
		return fmt.Errorf("stat WAL: %w", err)
	}
	if info.Size() == 0 {
		if err := writeAll(w.file, walMagic[:]); err != nil {
			return fmt.Errorf("initialize WAL: %w", err)
		}
		if err := w.file.Sync(); err != nil {
			return fmt.Errorf("sync WAL header: %w", err)
		}
		return nil
	}
	if info.Size() < walHeaderSize {
		return fmt.Errorf("lsm: WAL header is truncated")
	}

	var magic [walHeaderSize]byte
	if _, err := w.file.ReadAt(magic[:], 0); err != nil {
		return fmt.Errorf("read WAL header: %w", err)
	}
	if magic != walMagic {
		return fmt.Errorf("lsm: invalid WAL format")
	}

	offset := int64(walHeaderSize)
	for {
		recordStart := offset
		var header [walRecordHeaderSize]byte
		read, err := w.file.ReadAt(header[:], offset)
		if err == io.EOF && read == 0 {
			break
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			if err := w.truncateTail(recordStart); err != nil {
				return err
			}
			break
		}
		if err != nil {
			return fmt.Errorf("read WAL record header: %w", err)
		}

		payloadLength := binary.LittleEndian.Uint32(header[:4])
		expectedChecksum := binary.LittleEndian.Uint32(header[4:])
		if payloadLength < 9 || payloadLength > uint32(9+maxKeySize+maxValueSize) {
			return fmt.Errorf("lsm: invalid WAL record length %d", payloadLength)
		}
		payload := make([]byte, payloadLength)
		read, err = w.file.ReadAt(payload, offset+walRecordHeaderSize)
		if err == io.EOF || err == io.ErrUnexpectedEOF || read != len(payload) {
			if err := w.truncateTail(recordStart); err != nil {
				return err
			}
			break
		}
		if err != nil {
			return fmt.Errorf("read WAL record: %w", err)
		}
		if crc32.ChecksumIEEE(payload) != expectedChecksum {
			return fmt.Errorf("lsm: WAL checksum mismatch at offset %d", recordStart)
		}

		key, value, tombstone, err := decodeWALPayload(payload)
		if err != nil {
			return fmt.Errorf("decode WAL record at offset %d: %w", recordStart, err)
		}
		apply(string(key), mutation{value: value, tombstone: tombstone})
		offset += walRecordHeaderSize + int64(payloadLength)
	}

	if _, err := w.file.Seek(0, io.SeekEnd); err != nil {
		return fmt.Errorf("seek WAL: %w", err)
	}
	return nil
}

func decodeWALPayload(payload []byte) ([]byte, []byte, bool, error) {
	operation := payload[0]
	keyLength := binary.LittleEndian.Uint32(payload[1:5])
	valueLength := binary.LittleEndian.Uint32(payload[5:9])
	if keyLength == 0 || keyLength > maxKeySize || valueLength > maxValueSize {
		return nil, nil, false, fmt.Errorf("invalid key/value lengths")
	}
	if uint64(9)+uint64(keyLength)+uint64(valueLength) != uint64(len(payload)) {
		return nil, nil, false, fmt.Errorf("record length does not match payload")
	}
	if operation != walPut && operation != walDelete {
		return nil, nil, false, fmt.Errorf("unknown operation %d", operation)
	}
	if operation == walDelete && valueLength != 0 {
		return nil, nil, false, fmt.Errorf("delete record contains a value")
	}

	keyEnd := 9 + int(keyLength)
	key := cloneBytes(payload[9:keyEnd])
	value := cloneBytes(payload[keyEnd:])
	return key, value, operation == walDelete, nil
}

func (w *writeAheadLog) append(key, value []byte, tombstone bool) error {
	operation := walPut
	if tombstone {
		operation = walDelete
		value = nil
	}

	payload := bytes.NewBuffer(make([]byte, 0, 9+len(key)+len(value)))
	payload.WriteByte(operation)
	_ = binary.Write(payload, binary.LittleEndian, uint32(len(key)))
	_ = binary.Write(payload, binary.LittleEndian, uint32(len(value)))
	_, _ = payload.Write(key)
	_, _ = payload.Write(value)

	record := make([]byte, walRecordHeaderSize+payload.Len())
	binary.LittleEndian.PutUint32(record[:4], uint32(payload.Len()))
	binary.LittleEndian.PutUint32(record[4:8], crc32.ChecksumIEEE(payload.Bytes()))
	copy(record[walRecordHeaderSize:], payload.Bytes())

	start, err := w.file.Seek(0, io.SeekCurrent)
	if err != nil {
		return fmt.Errorf("locate WAL append: %w", err)
	}
	if err := writeAll(w.file, record); err != nil {
		_ = w.file.Truncate(start)
		_, _ = w.file.Seek(start, io.SeekStart)
		return fmt.Errorf("append WAL: %w", err)
	}
	if err := w.file.Sync(); err != nil {
		return fmt.Errorf("sync WAL: %w", err)
	}
	return nil
}

func (w *writeAheadLog) reset() error {
	directory := filepath.Dir(w.path)
	replacement, err := os.CreateTemp(directory, ".wal-tmp-*")
	if err != nil {
		return fmt.Errorf("create replacement WAL: %w", err)
	}
	replacementPath := replacement.Name()
	defer os.Remove(replacementPath)

	if err := writeAll(replacement, walMagic[:]); err != nil {
		_ = replacement.Close()
		return fmt.Errorf("rewrite WAL header: %w", err)
	}
	if err := replacement.Sync(); err != nil {
		_ = replacement.Close()
		return fmt.Errorf("sync reset WAL: %w", err)
	}
	if err := os.Rename(replacementPath, w.path); err != nil {
		_ = replacement.Close()
		return fmt.Errorf("install reset WAL: %w", err)
	}

	// Once rename succeeds, replacement is the durable log even if syncing the
	// directory or closing the old unlinked handle reports an error. Switch the
	// live handle before returning so later writes cannot target the old file.
	oldFile := w.file
	w.file = replacement
	directoryErr := syncDirectory(directory)
	closeErr := oldFile.Close()
	return errors.Join(directoryErr, closeErr)
}

func (w *writeAheadLog) truncateTail(size int64) error {
	if err := w.file.Truncate(size); err != nil {
		return fmt.Errorf("truncate incomplete WAL record: %w", err)
	}
	if err := w.file.Sync(); err != nil {
		return fmt.Errorf("sync truncated WAL: %w", err)
	}
	return nil
}

func (w *writeAheadLog) close() error {
	return w.file.Close()
}
