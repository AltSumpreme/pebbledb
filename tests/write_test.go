// ...existing code...
package tests

import (
	"fmt"
	"os"
	"pebbledb/db"
	"pebbledb/storage"
	"strconv"
	"testing"
	"time"
)

func TestWriteThroughput(t *testing.T) {
	// Benchmark test
	_ = os.RemoveAll(storage.DBDir)
	_ = os.MkdirAll(storage.DBDir, 0775)

	database := db.NewDatabase()
	if err := database.CreateTable("bench_users", []db.Column{
		{Name: "id", Type: db.TypeInt},
		{Name: "name", Type: db.TypeString},
	}); err != nil {
		t.Fatalf("CreateTable failed: %v", err)
	}

	const N = 200000 // No of ops
	start := time.Now()
	for i := 0; i < N; i++ {
		if err := database.InsertValue("bench_users", []string{
			strconv.Itoa(i),
			"user" + strconv.Itoa(i),
		}); err != nil {
			t.Fatalf("Insert failed at %d: %v", i, err)
		}
	}

	elapsed := time.Since(start)

	opsPerSec := float64(N) / elapsed.Seconds()
	fmt.Print(opsPerSec)
	t.Logf("Performed %d inserts in %v (%.2f ops/sec)", N, elapsed, opsPerSec)
	table, _ := database.GetTable("bench_users")

	t.Logf("Storage utilization: %.2f%%", table.StorageUtilization()*100)
	t.Logf("Active page utilization: %.2f%%", table.ActivePageUtilization()*100)
	t.Logf("Total pages: %d", len(table.PageNo))

	startSave := time.Now()
	if err := storage.SaveToDisk(database); err != nil {
		t.Fatalf("SaveToDisk failed: %v", err)
	}
	saveElapsed := time.Since(startSave)
	t.Logf("SaveToDisk took %v", saveElapsed)

}
