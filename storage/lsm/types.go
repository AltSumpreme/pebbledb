// Package lsm implements PebbleDB's durable, ordered key-value storage layer.
//
// It is an internal database building block. SQL users interact with tables and
// rows; catalog and row encoders will translate those objects into ordered keys.
package lsm

import "errors"

const (
	defaultMemtableSize = 4 << 20 // 4 MiB
	maxKeySize          = 1 << 20 // 1 MiB
	maxValueSize        = 64 << 20
)

var (
	ErrClosed        = errors.New("lsm: store is closed")
	ErrEmptyKey      = errors.New("lsm: key cannot be empty")
	ErrKeyTooLarge   = errors.New("lsm: key is too large")
	ErrValueTooLarge = errors.New("lsm: value is too large")
	ErrInvalidRange  = errors.New("lsm: range start must not sort after range end")
)

// Options controls a Store. Zero values select safe defaults.
type Options struct {
	// MemtableSizeBytes is the approximate amount of pending key/value data that
	// triggers an automatic flush to a new SSTable.
	MemtableSizeBytes int
}

func (o Options) normalized() (Options, error) {
	if o.MemtableSizeBytes < 0 {
		return Options{}, errors.New("lsm: memtable size cannot be negative")
	}
	if o.MemtableSizeBytes == 0 {
		o.MemtableSizeBytes = defaultMemtableSize
	}
	return o, nil
}

// Entry is a live key/value pair returned by Scan. Keys and values are owned by
// the caller and may be safely modified.
type Entry struct {
	Key   []byte
	Value []byte
}

// Stats is a point-in-time view of the local storage state.
type Stats struct {
	MemtableEntries int
	MemtableBytes   int
	SSTables        int
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
