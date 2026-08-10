// Package kv defines the internal ordered key/value contract shared by the
// relational, transactional, and storage layers. It is not a public database
// API; SQL remains PebbleDB's user-facing interface.
package kv

// Entry is a live key/value pair returned by Scan. Implementations return
// caller-owned byte slices.
type Entry struct {
	Key   []byte
	Value []byte
}

// Mutation is one operation in an atomic storage batch.
type Mutation struct {
	Key    []byte
	Value  []byte
	Delete bool
}

// Store is the ordered key/value interface consumed by higher layers.
type Store interface {
	Put(key, value []byte) error
	Get(key []byte) (value []byte, found bool, err error)
	Delete(key []byte) error
	Scan(start, end []byte) ([]Entry, error)
}

// BatchStore extends Store with an all-or-nothing durable batch.
type BatchStore interface {
	Store
	Apply(mutations []Mutation) error
}
