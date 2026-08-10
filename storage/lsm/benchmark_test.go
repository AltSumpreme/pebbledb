package lsm

import (
	"fmt"
	"testing"
)

func BenchmarkDurablePut(b *testing.B) {
	store, err := Open(b.TempDir(), Options{DisableBackgroundCompaction: true})
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()
	value := make([]byte, 256)
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if err := store.Put([]byte(fmt.Sprintf("row/%012d", index)), value); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(len(value)), "value_bytes/op")
}

func BenchmarkCachedPointRead(b *testing.B) {
	store, err := Open(b.TempDir(), Options{MemtableSizeBytes: 1, DisableBackgroundCompaction: true})
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()
	key, value := []byte("row/000000000001"), make([]byte, 1024)
	if err := store.Put(key, value); err != nil {
		b.Fatal(err)
	}
	_, _, _ = store.Get(key)
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, found, err := store.Get(key); err != nil || !found {
			b.Fatalf("get found=%v err=%v", found, err)
		}
	}
	b.ReportMetric(float64(len(value)), "value_bytes/op")
}

func BenchmarkOrderedScan1000(b *testing.B) {
	store, err := Open(b.TempDir(), Options{DisableBackgroundCompaction: true})
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()
	for index := 0; index < 1000; index++ {
		if err := store.Put([]byte(fmt.Sprintf("row/%04d", index)), []byte("value")); err != nil {
			b.Fatal(err)
		}
	}
	if err := store.Flush(); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		entries, err := store.Scan([]byte("row/"), []byte("row0"))
		if err != nil || len(entries) != 1000 {
			b.Fatalf("scan entries=%d err=%v", len(entries), err)
		}
	}
	b.ReportMetric(1000, "rows/op")
}
