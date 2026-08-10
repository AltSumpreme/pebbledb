package lsm

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"pebbledb/storage/kv"
)

const walFilename = "wal.log"

// Store is a durable ordered key-value store backed by a WAL, a memtable, and
// immutable SSTables. Store methods are safe for concurrent use.
type Store struct {
	mu                    sync.RWMutex
	directory             string
	options               Options
	wal                   *writeAheadLog
	memtable              map[string]mutation
	memtableBytes         int
	tables                []*sstable // oldest to newest
	nextGeneration        uint64
	closed                bool
	closing               bool
	cache                 *blockCache
	maintenance           chan struct{}
	stopMaintenance       chan struct{}
	maintenanceDone       sync.WaitGroup
	backgroundCompactions uint64
	lockFile              *os.File
}

// Open opens or creates a Store in directory. At most one Options value may be
// supplied. WAL records that were not flushed before a process stopped are
// replayed into the memtable.
func Open(directory string, supplied ...Options) (*Store, error) {
	if directory == "" {
		return nil, errors.New("lsm: directory cannot be empty")
	}
	if len(supplied) > 1 {
		return nil, errors.New("lsm: Open accepts at most one Options value")
	}
	options := Options{}
	if len(supplied) == 1 {
		options = supplied[0]
	}
	var err error
	options, err = options.normalized()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(directory, 0755); err != nil {
		return nil, fmt.Errorf("create LSM directory: %w", err)
	}
	lockFile, err := acquireDirectoryLock(directory)
	if err != nil {
		return nil, err
	}
	releaseLock := true
	defer func() {
		if releaseLock {
			_ = releaseDirectoryLock(lockFile)
		}
	}()
	if err := ensureFormatVersion(directory); err != nil {
		return nil, err
	}

	tables, maxGeneration, err := loadSSTables(directory, options.BloomBitsPerKey)
	if err != nil {
		return nil, err
	}
	store := &Store{
		directory:       directory,
		options:         options,
		memtable:        make(map[string]mutation),
		tables:          tables,
		nextGeneration:  maxGeneration + 1,
		cache:           newBlockCache(options.BlockCacheBytes),
		maintenance:     make(chan struct{}, 1),
		stopMaintenance: make(chan struct{}),
		lockFile:        lockFile,
	}
	if store.nextGeneration == 0 {
		closeSSTables(tables)
		return nil, errors.New("lsm: SSTable generation space exhausted")
	}

	store.wal, err = openWriteAheadLog(filepath.Join(directory, walFilename), store.applyMemtableBatch)
	if err != nil {
		closeSSTables(tables)
		return nil, err
	}
	if !options.DisableBackgroundCompaction {
		store.maintenanceDone.Add(1)
		go store.maintain()
	}
	releaseLock = false
	return store, nil
}

// Put durably associates key with value. An empty value is distinct from a
// deleted key. The input slices are copied before Put returns.
func (store *Store) Put(key, value []byte) error {
	if err := validateKeyValue(key, value); err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed || store.closing {
		return ErrClosed
	}
	if err := store.wal.append(key, value, false); err != nil {
		return err
	}
	store.applyMemtable(string(key), mutation{value: cloneBytes(value)})
	if store.memtableBytes >= store.options.MemtableSizeBytes {
		return store.flushLocked()
	}
	return nil
}

// Delete durably records a tombstone for key. Deleting a missing key succeeds.
func (store *Store) Delete(key []byte) error {
	if err := validateKey(key); err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed || store.closing {
		return ErrClosed
	}
	if err := store.wal.append(key, nil, true); err != nil {
		return err
	}
	store.applyMemtable(string(key), mutation{tombstone: true})
	if store.memtableBytes >= store.options.MemtableSizeBytes {
		return store.flushLocked()
	}
	return nil
}

// Apply durably installs every mutation as one atomic WAL record. Validation
// happens before the record is written, and recovery never replays a partial
// batch.
func (store *Store) Apply(mutations []kv.Mutation) error {
	if len(mutations) == 0 {
		return nil
	}
	entries := make([]walMutation, len(mutations))
	for index, current := range mutations {
		if current.Delete {
			if err := validateKey(current.Key); err != nil {
				return fmt.Errorf("lsm: batch mutation %d: %w", index+1, err)
			}
			entries[index] = walMutation{key: cloneBytes(current.Key), entry: mutation{tombstone: true}}
			continue
		}
		if err := validateKeyValue(current.Key, current.Value); err != nil {
			return fmt.Errorf("lsm: batch mutation %d: %w", index+1, err)
		}
		entries[index] = walMutation{key: cloneBytes(current.Key), entry: mutation{value: cloneBytes(current.Value)}}
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed || store.closing {
		return ErrClosed
	}
	if err := store.wal.appendBatch(entries); err != nil {
		return err
	}
	store.applyMemtableBatch(entries)
	if store.memtableBytes >= store.options.MemtableSizeBytes {
		return store.flushLocked()
	}
	return nil
}

// Get returns a copy of the latest value for key. found is false for both a
// missing key and a key hidden by a tombstone.
func (store *Store) Get(key []byte) (value []byte, found bool, err error) {
	if err := validateKey(key); err != nil {
		return nil, false, err
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	if store.closed || store.closing {
		return nil, false, ErrClosed
	}
	keyString := string(key)
	if entry, ok := store.memtable[keyString]; ok {
		if entry.tombstone {
			return nil, false, nil
		}
		return cloneBytes(entry.value), true, nil
	}
	for i := len(store.tables) - 1; i >= 0; i-- {
		entry, ok, err := store.tables[i].get(keyString, store.cache)
		if err != nil {
			return nil, false, err
		}
		if !ok {
			continue
		}
		if entry.tombstone {
			return nil, false, nil
		}
		return cloneBytes(entry.value), true, nil
	}
	return nil, false, nil
}

// Scan returns live entries in bytewise key order. start is inclusive and end
// is exclusive. A nil or empty bound is unbounded.
func (store *Store) Scan(start, end []byte) ([]Entry, error) {
	if len(start) > maxKeySize || len(end) > maxKeySize {
		return nil, ErrKeyTooLarge
	}
	startKey, endKey := string(start), string(end)
	if endKey != "" && startKey > endKey {
		return nil, ErrInvalidRange
	}

	store.mu.RLock()
	defer store.mu.RUnlock()
	if store.closed || store.closing {
		return nil, ErrClosed
	}
	resolved := make(map[string]mutation)
	for _, table := range store.tables {
		entries, err := table.mutations(startKey, endKey)
		if err != nil {
			return nil, err
		}
		for key, entry := range entries {
			resolved[key] = entry
		}
	}
	for key, entry := range store.memtable {
		if keyInRange(key, startKey, endKey) {
			resolved[key] = entry
		}
	}

	keys := make([]string, 0, len(resolved))
	for key, entry := range resolved {
		if !entry.tombstone {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	result := make([]Entry, 0, len(keys))
	for _, key := range keys {
		result = append(result, Entry{
			Key:   []byte(key),
			Value: cloneBytes(resolved[key].value),
		})
	}
	return result, nil
}

// Flush writes the current memtable to an immutable SSTable and resets the WAL.
func (store *Store) Flush() error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed || store.closing {
		return ErrClosed
	}
	return store.flushLocked()
}

func (store *Store) flushLocked() error {
	if len(store.memtable) == 0 {
		return nil
	}
	table, err := writeSSTable(store.directory, store.nextGeneration, store.memtable, store.options.BloomBitsPerKey)
	if err != nil {
		return err
	}
	store.nextGeneration++
	if store.nextGeneration == 0 {
		_ = table.close()
		return errors.New("lsm: SSTable generation space exhausted")
	}
	store.tables = append(store.tables, table)

	// The SSTable is durable before the WAL is cleared. A crash between these
	// operations can replay a duplicate, but cannot lose the latest value.
	if err := store.wal.reset(); err != nil {
		return err
	}
	store.memtable = make(map[string]mutation)
	store.memtableBytes = 0
	store.requestMaintenanceLocked()
	return nil
}

// Compact merges all immutable tables into one newest table. Tombstones are
// retained so an interrupted cleanup cannot expose values from an older table.
func (store *Store) Compact() error {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed || store.closing {
		return ErrClosed
	}
	if err := store.flushLocked(); err != nil {
		return err
	}
	if len(store.tables) <= 1 {
		return nil
	}

	resolved := make(map[string]mutation)
	for _, table := range store.tables {
		entries, err := table.mutations("", "")
		if err != nil {
			return err
		}
		for key, entry := range entries {
			resolved[key] = entry
		}
	}
	compacted, err := writeSSTable(store.directory, store.nextGeneration, resolved, store.options.BloomBitsPerKey)
	if err != nil {
		return err
	}
	store.nextGeneration++
	if store.nextGeneration == 0 {
		_ = compacted.close()
		return errors.New("lsm: SSTable generation space exhausted")
	}

	oldTables := store.tables
	store.tables = []*sstable{compacted}
	var cleanupErrors []error
	for _, table := range oldTables {
		store.cache.removeGeneration(table.generation)
		if err := table.close(); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("close old SSTable: %w", err))
			continue
		}
		if err := os.Remove(table.path); err != nil && !os.IsNotExist(err) {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("remove old SSTable %s: %w", filepath.Base(table.path), err))
		}
	}
	if err := syncDirectory(store.directory); err != nil {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("sync compacted directory: %w", err))
	}
	return errors.Join(cleanupErrors...)
}

// Checkpoint creates a self-contained, point-in-time copy of the store at
// destination. It never overwrites an existing path. Files are copied into a
// sibling temporary directory and atomically renamed after every file and
// directory entry is durable.
func (store *Store) Checkpoint(destination string) error {
	if destination == "" {
		return fmt.Errorf("lsm: checkpoint destination cannot be empty")
	}
	absolute, err := filepath.Abs(destination)
	if err != nil {
		return fmt.Errorf("lsm: resolve checkpoint destination: %w", err)
	}
	sourceAbsolute, err := filepath.Abs(store.directory)
	if err != nil {
		return fmt.Errorf("lsm: resolve checkpoint source: %w", err)
	}
	relative, err := filepath.Rel(sourceAbsolute, absolute)
	if err != nil {
		return fmt.Errorf("lsm: compare checkpoint paths: %w", err)
	}
	if relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator))) {
		return fmt.Errorf("lsm: checkpoint destination cannot be inside source")
	}
	if _, err := os.Lstat(absolute); err == nil {
		return fmt.Errorf("lsm: checkpoint destination already exists")
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("lsm: inspect checkpoint destination: %w", err)
	}
	parent := filepath.Dir(absolute)
	if err := os.MkdirAll(parent, 0755); err != nil {
		return fmt.Errorf("lsm: create checkpoint parent: %w", err)
	}
	temporary, err := os.MkdirTemp(parent, ".pebbledb-checkpoint-*")
	if err != nil {
		return fmt.Errorf("lsm: create checkpoint staging directory: %w", err)
	}
	installed := false
	defer func() {
		if !installed {
			_ = os.RemoveAll(temporary)
		}
	}()

	store.mu.Lock()
	defer store.mu.Unlock()
	if store.closed || store.closing {
		return ErrClosed
	}
	if err := store.flushLocked(); err != nil {
		return fmt.Errorf("lsm: flush checkpoint: %w", err)
	}
	entries, err := os.ReadDir(store.directory)
	if err != nil {
		return fmt.Errorf("lsm: list checkpoint source: %w", err)
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		if err := copyDurableFile(filepath.Join(store.directory, entry.Name()), filepath.Join(temporary, entry.Name())); err != nil {
			return err
		}
	}
	if err := syncDirectory(temporary); err != nil {
		return fmt.Errorf("lsm: sync checkpoint staging directory: %w", err)
	}
	if err := os.Rename(temporary, absolute); err != nil {
		return fmt.Errorf("lsm: install checkpoint: %w", err)
	}
	installed = true
	if err := syncDirectory(parent); err != nil {
		return fmt.Errorf("lsm: sync checkpoint parent: %w", err)
	}
	return nil
}

func copyDurableFile(source, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("lsm: open checkpoint source %s: %w", filepath.Base(source), err)
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("lsm: create checkpoint file %s: %w", filepath.Base(destination), err)
	}
	if _, err := io.Copy(output, input); err != nil {
		_ = output.Close()
		return fmt.Errorf("lsm: copy checkpoint file %s: %w", filepath.Base(source), err)
	}
	if err := output.Sync(); err != nil {
		_ = output.Close()
		return fmt.Errorf("lsm: sync checkpoint file %s: %w", filepath.Base(source), err)
	}
	if err := output.Close(); err != nil {
		return fmt.Errorf("lsm: close checkpoint file %s: %w", filepath.Base(source), err)
	}
	return nil
}

// Stats returns local counts useful for diagnostics and tests.
func (store *Store) Stats() Stats {
	store.mu.RLock()
	defer store.mu.RUnlock()
	cacheEntries, cacheBytes, cacheHits, cacheMisses := store.cache.stats()
	var bloomRejections uint64
	for _, table := range store.tables {
		bloomRejections += table.bloomRejections.Load()
	}
	return Stats{
		MemtableEntries: len(store.memtable),
		MemtableBytes:   store.memtableBytes,
		SSTables:        len(store.tables),
		CacheEntries:    cacheEntries, CacheBytes: cacheBytes,
		CacheHits: cacheHits, CacheMisses: cacheMisses,
		BloomRejections:       bloomRejections,
		BackgroundCompactions: store.backgroundCompactions,
	}
}

// Close flushes pending writes and releases file handles. It is idempotent.
func (store *Store) Close() error {
	store.mu.Lock()
	if store.closed || store.closing {
		store.mu.Unlock()
		return nil
	}
	store.closing = true
	store.mu.Unlock()
	close(store.stopMaintenance)
	store.maintenanceDone.Wait()
	store.mu.Lock()
	defer store.mu.Unlock()
	var closeErrors []error
	if err := store.flushLocked(); err != nil {
		closeErrors = append(closeErrors, err)
	}
	if err := store.wal.close(); err != nil {
		closeErrors = append(closeErrors, fmt.Errorf("close WAL: %w", err))
	}
	for _, table := range store.tables {
		if err := table.close(); err != nil {
			closeErrors = append(closeErrors, fmt.Errorf("close SSTable: %w", err))
		}
	}
	if err := releaseDirectoryLock(store.lockFile); err != nil {
		closeErrors = append(closeErrors, fmt.Errorf("release directory lock: %w", err))
	}
	store.lockFile = nil
	store.closed = true
	store.closing = false
	return errors.Join(closeErrors...)
}

func (store *Store) requestMaintenanceLocked() {
	if store.options.DisableBackgroundCompaction || len(store.tables) < store.options.CompactionThreshold {
		return
	}
	select {
	case store.maintenance <- struct{}{}:
	default:
	}
}

func (store *Store) maintain() {
	defer store.maintenanceDone.Done()
	for {
		select {
		case <-store.maintenance:
			if err := store.Compact(); err == nil {
				store.mu.Lock()
				store.backgroundCompactions++
				store.mu.Unlock()
			}
		case <-store.stopMaintenance:
			return
		}
	}
}

func (store *Store) applyMemtable(key string, entry mutation) {
	if previous, exists := store.memtable[key]; exists {
		store.memtableBytes -= mutationSize(key, previous)
	}
	entry.value = cloneBytes(entry.value)
	store.memtable[key] = entry
	store.memtableBytes += mutationSize(key, entry)
}

func (store *Store) applyMemtableBatch(entries []walMutation) {
	for _, current := range entries {
		store.applyMemtable(string(current.key), current.entry)
	}
}

func mutationSize(key string, entry mutation) int {
	return len(key) + len(entry.value) + 1
}

func validateKey(key []byte) error {
	if len(key) == 0 {
		return ErrEmptyKey
	}
	if len(key) > maxKeySize {
		return ErrKeyTooLarge
	}
	return nil
}

func validateKeyValue(key, value []byte) error {
	if err := validateKey(key); err != nil {
		return err
	}
	if len(value) > maxValueSize {
		return ErrValueTooLarge
	}
	return nil
}

func keyInRange(key, start, end string) bool {
	return (start == "" || key >= start) && (end == "" || key < end)
}
