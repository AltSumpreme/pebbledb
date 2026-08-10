package lsm

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"os"
	"path/filepath"
)

const (
	walPut              byte = 1
	walDelete           byte = 2
	walBatch            byte = 3
	walHeaderSize            = 8
	walRecordHeaderSize      = 8
	maxWALBatchSize          = 256 << 20
)

type walMutation struct {
	key   []byte
	entry mutation
}

type writeAheadLog struct {
	file *os.File
	path string
}

func openWriteAheadLog(path string, apply func([]walMutation)) (*writeAheadLog, error) {
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

func (w *writeAheadLog) initializeAndReplay(apply func([]walMutation)) error {
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
		if payloadLength == 0 || payloadLength > uint32(maxWALBatchSize) {
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

		mutations, err := decodeWALPayload(payload)
		if err != nil {
			return fmt.Errorf("decode WAL record at offset %d: %w", recordStart, err)
		}
		apply(mutations)
		offset += walRecordHeaderSize + int64(payloadLength)
	}

	if _, err := w.file.Seek(0, io.SeekEnd); err != nil {
		return fmt.Errorf("seek WAL: %w", err)
	}
	return nil
}

func decodeWALPayload(payload []byte) ([]walMutation, error) {
	if len(payload) == 0 {
		return nil, fmt.Errorf("empty payload")
	}
	if payload[0] == walBatch {
		return decodeWALBatch(payload)
	}
	current, consumed, err := decodeWALMutation(payload)
	if err != nil {
		return nil, err
	}
	if consumed != len(payload) {
		return nil, fmt.Errorf("record length does not match payload")
	}
	return []walMutation{current}, nil
}

func decodeWALBatch(payload []byte) ([]walMutation, error) {
	if len(payload) < 5 {
		return nil, fmt.Errorf("batch header is truncated")
	}
	count := binary.LittleEndian.Uint32(payload[1:5])
	if count == 0 {
		return nil, fmt.Errorf("batch is empty")
	}
	result := make([]walMutation, 0, count)
	offset := 5
	for index := uint32(0); index < count; index++ {
		if offset >= len(payload) {
			return nil, fmt.Errorf("batch mutation %d is truncated", index+1)
		}
		current, consumed, err := decodeWALMutation(payload[offset:])
		if err != nil {
			return nil, fmt.Errorf("batch mutation %d: %w", index+1, err)
		}
		result = append(result, current)
		offset += consumed
	}
	if offset != len(payload) {
		return nil, fmt.Errorf("batch length does not match payload")
	}
	return result, nil
}

func decodeWALMutation(payload []byte) (walMutation, int, error) {
	if len(payload) < 9 {
		return walMutation{}, 0, fmt.Errorf("mutation header is truncated")
	}
	operation := payload[0]
	keyLength := binary.LittleEndian.Uint32(payload[1:5])
	valueLength := binary.LittleEndian.Uint32(payload[5:9])
	if keyLength == 0 || keyLength > maxKeySize || valueLength > maxValueSize {
		return walMutation{}, 0, fmt.Errorf("invalid key/value lengths")
	}
	length := uint64(9) + uint64(keyLength) + uint64(valueLength)
	if length > uint64(len(payload)) {
		return walMutation{}, 0, fmt.Errorf("mutation is truncated")
	}
	if operation != walPut && operation != walDelete {
		return walMutation{}, 0, fmt.Errorf("unknown operation %d", operation)
	}
	if operation == walDelete && valueLength != 0 {
		return walMutation{}, 0, fmt.Errorf("delete record contains a value")
	}

	keyEnd := 9 + int(keyLength)
	key := cloneBytes(payload[9:keyEnd])
	valueEnd := keyEnd + int(valueLength)
	value := cloneBytes(payload[keyEnd:valueEnd])
	return walMutation{key: key, entry: mutation{value: value, tombstone: operation == walDelete}}, int(length), nil
}

func (w *writeAheadLog) append(key, value []byte, tombstone bool) error {
	operation := walPut
	if tombstone {
		operation = walDelete
		value = nil
	}

	payload := bytes.NewBuffer(make([]byte, 0, 9+len(key)+len(value)))
	encodeWALMutation(payload, operation, key, value)
	return w.appendPayload(payload.Bytes())
}

func (w *writeAheadLog) appendBatch(mutations []walMutation) error {
	length := uint64(5)
	for _, current := range mutations {
		length += uint64(9 + len(current.key) + len(current.entry.value))
	}
	if length > maxWALBatchSize || length > math.MaxUint32 {
		return fmt.Errorf("lsm: WAL batch is too large")
	}
	payload := bytes.NewBuffer(make([]byte, 0, int(length)))
	payload.WriteByte(walBatch)
	_ = binary.Write(payload, binary.LittleEndian, uint32(len(mutations)))
	for _, current := range mutations {
		operation := walPut
		value := current.entry.value
		if current.entry.tombstone {
			operation = walDelete
			value = nil
		}
		encodeWALMutation(payload, operation, current.key, value)
	}
	return w.appendPayload(payload.Bytes())
}

func encodeWALMutation(payload *bytes.Buffer, operation byte, key, value []byte) {
	payload.WriteByte(operation)
	_ = binary.Write(payload, binary.LittleEndian, uint32(len(key)))
	_ = binary.Write(payload, binary.LittleEndian, uint32(len(value)))
	_, _ = payload.Write(key)
	_, _ = payload.Write(value)
}

func (w *writeAheadLog) appendPayload(payload []byte) error {
	record := make([]byte, walRecordHeaderSize+len(payload))
	binary.LittleEndian.PutUint32(record[:4], uint32(len(payload)))
	binary.LittleEndian.PutUint32(record[4:8], crc32.ChecksumIEEE(payload))
	copy(record[walRecordHeaderSize:], payload)

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
