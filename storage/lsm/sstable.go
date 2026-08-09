package lsm

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	sstHeaderSize       = 16
	sstRecordHeaderSize = 12
	sstPrefix           = "sst-"
	sstSuffix           = ".sst"
)

type sstableIndexEntry struct {
	key         string
	valueOffset int64
	valueLength int32
	checksum    uint32
}

type sstable struct {
	generation uint64
	path       string
	file       *os.File
	index      []sstableIndexEntry
}

func loadSSTables(directory string) ([]*sstable, uint64, error) {
	directoryEntries, err := os.ReadDir(directory)
	if err != nil {
		return nil, 0, fmt.Errorf("read LSM directory: %w", err)
	}

	type tableFile struct {
		generation uint64
		path       string
	}
	var files []tableFile
	for _, entry := range directoryEntries {
		if entry.IsDir() {
			continue
		}
		generation, matches, err := parseSSTableName(entry.Name())
		if err != nil {
			return nil, 0, err
		}
		if matches {
			files = append(files, tableFile{
				generation: generation,
				path:       filepath.Join(directory, entry.Name()),
			})
		}
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].generation < files[j].generation
	})

	tables := make([]*sstable, 0, len(files))
	var maxGeneration uint64
	for _, file := range files {
		table, err := openSSTable(file.path, file.generation)
		if err != nil {
			closeSSTables(tables)
			return nil, 0, err
		}
		tables = append(tables, table)
		maxGeneration = file.generation
	}
	return tables, maxGeneration, nil
}

func parseSSTableName(name string) (uint64, bool, error) {
	if !strings.HasPrefix(name, sstPrefix) || !strings.HasSuffix(name, sstSuffix) {
		return 0, false, nil
	}
	rawGeneration := strings.TrimSuffix(strings.TrimPrefix(name, sstPrefix), sstSuffix)
	if rawGeneration == "" {
		return 0, false, fmt.Errorf("lsm: invalid SSTable name %q", name)
	}
	generation, err := strconv.ParseUint(rawGeneration, 10, 64)
	if err != nil || generation == 0 {
		return 0, false, fmt.Errorf("lsm: invalid SSTable name %q", name)
	}
	return generation, true, nil
}

func openSSTable(path string, generation uint64) (*sstable, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open SSTable %s: %w", filepath.Base(path), err)
	}
	fail := func(err error) (*sstable, error) {
		_ = file.Close()
		return nil, err
	}

	info, err := file.Stat()
	if err != nil {
		return fail(fmt.Errorf("stat SSTable %s: %w", filepath.Base(path), err))
	}
	if info.Size() < sstHeaderSize {
		return fail(fmt.Errorf("lsm: SSTable %s has a truncated header", filepath.Base(path)))
	}

	var header [sstHeaderSize]byte
	if _, err := file.ReadAt(header[:], 0); err != nil {
		return fail(fmt.Errorf("read SSTable header: %w", err))
	}
	var magic [8]byte
	copy(magic[:], header[:8])
	if magic != sstMagic {
		return fail(fmt.Errorf("lsm: invalid SSTable format in %s", filepath.Base(path)))
	}
	recordCount := binary.LittleEndian.Uint64(header[8:])
	if recordCount > uint64((info.Size()-sstHeaderSize)/sstRecordHeaderSize) {
		return fail(fmt.Errorf("lsm: impossible record count in %s", filepath.Base(path)))
	}

	table := &sstable{
		generation: generation,
		path:       path,
		file:       file,
		index:      make([]sstableIndexEntry, 0, recordCount),
	}
	offset := int64(sstHeaderSize)
	var previousKey string
	for recordNumber := uint64(0); recordNumber < recordCount; recordNumber++ {
		var recordHeader [sstRecordHeaderSize]byte
		if _, err := file.ReadAt(recordHeader[:], offset); err != nil {
			return fail(fmt.Errorf("read SSTable record %d: %w", recordNumber, err))
		}
		keyLength := binary.LittleEndian.Uint32(recordHeader[:4])
		valueLength := int32(binary.LittleEndian.Uint32(recordHeader[4:8]))
		checksum := binary.LittleEndian.Uint32(recordHeader[8:])
		if keyLength == 0 || keyLength > maxKeySize {
			return fail(fmt.Errorf("lsm: invalid key length in SSTable record %d", recordNumber))
		}
		if valueLength < -1 || valueLength > maxValueSize {
			return fail(fmt.Errorf("lsm: invalid value length in SSTable record %d", recordNumber))
		}
		storedValueLength := int64(valueLength)
		if valueLength == -1 {
			storedValueLength = 0
		}
		recordEnd := offset + sstRecordHeaderSize + int64(keyLength) + storedValueLength
		if recordEnd > info.Size() {
			return fail(fmt.Errorf("lsm: truncated SSTable record %d", recordNumber))
		}

		key := make([]byte, keyLength)
		if _, err := file.ReadAt(key, offset+sstRecordHeaderSize); err != nil {
			return fail(fmt.Errorf("read SSTable key %d: %w", recordNumber, err))
		}
		valueOffset := offset + sstRecordHeaderSize + int64(keyLength)
		var value []byte
		if storedValueLength > 0 {
			value = make([]byte, storedValueLength)
			if _, err := file.ReadAt(value, valueOffset); err != nil {
				return fail(fmt.Errorf("read SSTable value %d: %w", recordNumber, err))
			}
		}
		if checksumEntry(key, value, valueLength == -1) != checksum {
			return fail(fmt.Errorf("lsm: SSTable checksum mismatch in record %d", recordNumber))
		}

		keyString := string(key)
		if recordNumber > 0 && keyString <= previousKey {
			return fail(fmt.Errorf("lsm: SSTable keys are not strictly ordered"))
		}
		previousKey = keyString
		table.index = append(table.index, sstableIndexEntry{
			key:         keyString,
			valueOffset: valueOffset,
			valueLength: valueLength,
			checksum:    checksum,
		})
		offset = recordEnd
	}
	if offset != info.Size() {
		return fail(fmt.Errorf("lsm: SSTable %s contains trailing data", filepath.Base(path)))
	}
	return table, nil
}

func writeSSTable(directory string, generation uint64, records map[string]mutation) (*sstable, error) {
	keys := make([]string, 0, len(records))
	for key := range records {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	temporary, err := os.CreateTemp(directory, ".sst-tmp-*")
	if err != nil {
		return nil, fmt.Errorf("create temporary SSTable: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)

	buffered := bufio.NewWriter(temporary)
	var header [sstHeaderSize]byte
	copy(header[:8], sstMagic[:])
	binary.LittleEndian.PutUint64(header[8:], uint64(len(keys)))
	if err := writeAll(buffered, header[:]); err != nil {
		_ = temporary.Close()
		return nil, fmt.Errorf("write SSTable header: %w", err)
	}

	for _, key := range keys {
		entry := records[key]
		valueLength := int32(len(entry.value))
		if entry.tombstone {
			valueLength = -1
		}
		var recordHeader [sstRecordHeaderSize]byte
		binary.LittleEndian.PutUint32(recordHeader[:4], uint32(len(key)))
		binary.LittleEndian.PutUint32(recordHeader[4:8], uint32(valueLength))
		binary.LittleEndian.PutUint32(recordHeader[8:], checksumEntry([]byte(key), entry.value, entry.tombstone))
		if err := writeAll(buffered, recordHeader[:]); err != nil {
			_ = temporary.Close()
			return nil, fmt.Errorf("write SSTable record header: %w", err)
		}
		if err := writeAll(buffered, []byte(key)); err != nil {
			_ = temporary.Close()
			return nil, fmt.Errorf("write SSTable key: %w", err)
		}
		if !entry.tombstone {
			if err := writeAll(buffered, entry.value); err != nil {
				_ = temporary.Close()
				return nil, fmt.Errorf("write SSTable value: %w", err)
			}
		}
	}
	if err := buffered.Flush(); err != nil {
		_ = temporary.Close()
		return nil, fmt.Errorf("flush SSTable: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return nil, fmt.Errorf("sync SSTable: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return nil, fmt.Errorf("close SSTable: %w", err)
	}

	finalPath := filepath.Join(directory, fmt.Sprintf("%s%020d%s", sstPrefix, generation, sstSuffix))
	if err := os.Rename(temporaryPath, finalPath); err != nil {
		return nil, fmt.Errorf("install SSTable: %w", err)
	}
	if err := syncDirectory(directory); err != nil {
		return nil, fmt.Errorf("sync SSTable directory: %w", err)
	}
	return openSSTable(finalPath, generation)
}

func (table *sstable) get(key string) (mutation, bool, error) {
	position := sort.Search(len(table.index), func(i int) bool {
		return table.index[i].key >= key
	})
	if position == len(table.index) || table.index[position].key != key {
		return mutation{}, false, nil
	}
	entry, err := table.readMutation(table.index[position])
	return entry, true, err
}

func (table *sstable) mutations(start, end string) (map[string]mutation, error) {
	result := make(map[string]mutation)
	position := 0
	if start != "" {
		position = sort.Search(len(table.index), func(i int) bool {
			return table.index[i].key >= start
		})
	}
	for ; position < len(table.index); position++ {
		indexed := table.index[position]
		if end != "" && indexed.key >= end {
			break
		}
		entry, err := table.readMutation(indexed)
		if err != nil {
			return nil, err
		}
		result[indexed.key] = entry
	}
	return result, nil
}

func (table *sstable) readMutation(indexed sstableIndexEntry) (mutation, error) {
	if indexed.valueLength == -1 {
		return mutation{tombstone: true}, nil
	}
	value := make([]byte, indexed.valueLength)
	if len(value) > 0 {
		if _, err := table.file.ReadAt(value, indexed.valueOffset); err != nil {
			return mutation{}, fmt.Errorf("read value for key %q: %w", indexed.key, err)
		}
	}
	if checksumEntry([]byte(indexed.key), value, false) != indexed.checksum {
		return mutation{}, fmt.Errorf("lsm: SSTable checksum changed for key %q", indexed.key)
	}
	return mutation{value: value}, nil
}

func (table *sstable) close() error {
	return table.file.Close()
}

func closeSSTables(tables []*sstable) {
	for _, table := range tables {
		_ = table.close()
	}
}
