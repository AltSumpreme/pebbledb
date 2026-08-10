package lsm

import (
	"container/list"
	"encoding/binary"
	"hash/crc32"
	"sync"
	"sync/atomic"
)

type bloomFilter struct {
	bits []byte
	k    uint8
}

func newBloomFilter(keys []string, bitsPerKey int) *bloomFilter {
	if len(keys) == 0 || bitsPerKey <= 0 {
		return nil
	}
	bitCount := len(keys) * bitsPerKey
	if bitCount < 64 {
		bitCount = 64
	}
	filter := &bloomFilter{bits: make([]byte, (bitCount+7)/8), k: uint8(bitsPerKey * 69 / 100)}
	if filter.k < 1 {
		filter.k = 1
	}
	if filter.k > 30 {
		filter.k = 30
	}
	for _, key := range keys {
		filter.add([]byte(key))
	}
	return filter
}

func (filter *bloomFilter) add(key []byte) {
	first, second := bloomHashes(key)
	bits := uint32(len(filter.bits) * 8)
	for index := uint8(0); index < filter.k; index++ {
		position := (first + uint32(index)*second) % bits
		filter.bits[position/8] |= 1 << (position % 8)
	}
}

func (filter *bloomFilter) mayContain(key []byte) bool {
	if filter == nil {
		return true
	}
	first, second := bloomHashes(key)
	bits := uint32(len(filter.bits) * 8)
	for index := uint8(0); index < filter.k; index++ {
		position := (first + uint32(index)*second) % bits
		if filter.bits[position/8]&(1<<(position%8)) == 0 {
			return false
		}
	}
	return true
}

func bloomHashes(key []byte) (uint32, uint32) {
	first := crc32.ChecksumIEEE(key)
	var seed [4]byte
	binary.LittleEndian.PutUint32(seed[:], first^0x9e3779b9)
	secondHash := crc32.New(crc32.MakeTable(crc32.Castagnoli))
	_, _ = secondHash.Write(seed[:])
	_, _ = secondHash.Write(key)
	second := secondHash.Sum32() | 1
	return first, second
}

type cacheKey struct {
	generation uint64
	key        string
}

type cacheValue struct {
	key      cacheKey
	mutation mutation
	size     int
}

type blockCache struct {
	mu       sync.Mutex
	capacity int
	bytes    int
	items    map[cacheKey]*list.Element
	recency  *list.List
	hits     atomic.Uint64
	misses   atomic.Uint64
}

func newBlockCache(capacity int) *blockCache {
	return &blockCache{capacity: capacity, items: make(map[cacheKey]*list.Element), recency: list.New()}
}

func (cache *blockCache) get(key cacheKey) (mutation, bool) {
	if cache == nil || cache.capacity == 0 {
		return mutation{}, false
	}
	cache.mu.Lock()
	element := cache.items[key]
	if element == nil {
		cache.mu.Unlock()
		cache.misses.Add(1)
		return mutation{}, false
	}
	cache.recency.MoveToFront(element)
	value := element.Value.(cacheValue).mutation
	value.value = cloneBytes(value.value)
	cache.mu.Unlock()
	cache.hits.Add(1)
	return value, true
}

func (cache *blockCache) put(key cacheKey, value mutation) {
	if cache == nil || cache.capacity == 0 {
		return
	}
	size := len(key.key) + len(value.value) + 32
	if size > cache.capacity {
		return
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if existing := cache.items[key]; existing != nil {
		cache.bytes -= existing.Value.(cacheValue).size
		cache.recency.Remove(existing)
	}
	value.value = cloneBytes(value.value)
	element := cache.recency.PushFront(cacheValue{key: key, mutation: value, size: size})
	cache.items[key] = element
	cache.bytes += size
	for cache.bytes > cache.capacity {
		oldest := cache.recency.Back()
		current := oldest.Value.(cacheValue)
		delete(cache.items, current.key)
		cache.bytes -= current.size
		cache.recency.Remove(oldest)
	}
}

func (cache *blockCache) removeGeneration(generation uint64) {
	if cache == nil {
		return
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	for key, element := range cache.items {
		if key.generation == generation {
			cache.bytes -= element.Value.(cacheValue).size
			cache.recency.Remove(element)
			delete(cache.items, key)
		}
	}
}

func (cache *blockCache) stats() (entries, bytesValue int, hits, misses uint64) {
	if cache == nil {
		return
	}
	cache.mu.Lock()
	entries, bytesValue = len(cache.items), cache.bytes
	cache.mu.Unlock()
	return entries, bytesValue, cache.hits.Load(), cache.misses.Load()
}
