// Package lsm implements PebbleDB's durable, ordered key-value storage layer.
//
// It is an internal database building block. SQL users interact with tables and
// rows; catalog and row encoders will translate those objects into ordered keys.
package lsm

import (
	"errors"

	"pebbledb/storage/kv"
)

const (
	defaultMemtableSize = 4 << 20 // 4 MiB
	defaultBlockCache   = 16 << 20
	maxKeySize          = 1 << 20 // 1 MiB
	maxValueSize        = 64 << 20
)

var (
	ErrClosed        = errors.New("lsm: store is closed")
	ErrEmptyKey      = errors.New("lsm: key cannot be empty")
	ErrKeyTooLarge   = errors.New("lsm: key is too large")
	ErrValueTooLarge = errors.New("lsm: value is too large")
	ErrInvalidRange  = errors.New("lsm: range start must not sort after range end")
	ErrLocked        = errors.New("lsm: directory is already open by another process or store")
)

// Options controls a Store. Zero values select safe defaults.
type Options struct {
	// MemtableSizeBytes is the approximate amount of pending key/value data that
	// triggers an automatic flush to a new SSTable.
	MemtableSizeBytes int
	// BlockCacheBytes bounds cached SSTable values. Zero selects 16 MiB.
	BlockCacheBytes int
	// BloomBitsPerKey controls in-memory SSTable Bloom filters. Zero selects 10.
	BloomBitsPerKey int
	// CompactionThreshold schedules background compaction at this table count.
	// Zero selects 8. Set DisableBackgroundCompaction for manual maintenance.
	CompactionThreshold         int
	DisableBackgroundCompaction bool
}

func (o Options) normalized() (Options, error) {
	if o.MemtableSizeBytes < 0 {
		return Options{}, errors.New("lsm: memtable size cannot be negative")
	}
	if o.BlockCacheBytes < 0 || o.BloomBitsPerKey < 0 || o.BloomBitsPerKey > 64 || o.CompactionThreshold < 0 {
		return Options{}, errors.New("lsm: cache, Bloom, and compaction options cannot be negative or invalid")
	}
	if o.MemtableSizeBytes == 0 {
		o.MemtableSizeBytes = defaultMemtableSize
	}
	if o.BlockCacheBytes == 0 {
		o.BlockCacheBytes = defaultBlockCache
	}
	if o.BloomBitsPerKey == 0 {
		o.BloomBitsPerKey = 10
	}
	if o.CompactionThreshold == 0 {
		o.CompactionThreshold = 8
	}
	return o, nil
}

// Entry is kept as an alias for compatibility with callers of the storage
// package. The shared definition lets transactional views implement the same
// ordered KV contract.
type Entry = kv.Entry

// Stats is a point-in-time view of the local storage state.
type Stats struct {
	MemtableEntries       int
	MemtableBytes         int
	SSTables              int
	CacheEntries          int
	CacheBytes            int
	CacheHits             uint64
	CacheMisses           uint64
	BloomRejections       uint64
	BackgroundCompactions uint64
}

type mutation struct {
	value     []byte
	tombstone bool
}

func cloneBytes(value []byte) []byte {
	if value == nil {
		return nil
	}
	cloned := make([]byte, len(value))
	copy(cloned, value)
	return cloned
}
