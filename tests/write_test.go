package tests

import (
	"fmt"
	"os"
	"pebbledb/db"
	"pebbledb/storage"
	"strconv"
	"testing"
)

func TestWriteAndReadMultiPage(t *testing.T) {
	_ = os.RemoveAll(storage.DBDir)
	_ = os.MkdirAll(storage.DBDir, 0775)

	// Initialize and create the table
	database := db.NewDatabase()
	err := database.CreateTable("users", []db.Column{
		{Name: "id", Type: db.TypeInt},
		{Name: "name", Type: db.TypeString},
	})
	if err != nil {
		t.Fatalf("Failed to create table: %v", err)
	}

	// Insert 300 rows
	for i := 0; i < 300; i++ {
		err := database.InsertValue("users", []string{
			strconv.Itoa(i),
			"user" + strconv.Itoa(i),
		})
		if err != nil {
			t.Fatalf("Insert failed at row %d: %v", i, err)
		}
	}

	// Save database to disk
	if err := storage.SaveToDisk(database); err != nil {
		t.Fatalf("Failed to save to disk: %v", err)
	}

	// Load the database from disk
	loadedDB, err := storage.LoadFromDisk()
	if err != nil {
		t.Fatalf("Failed to load from disk: %v", err)
	}

	// Validate: ensure all 300 rows are retrievable and correct
	rows, err := loadedDB.SelectAll("users")
	if err != nil {
		t.Fatalf("Select failed: %v", err)
	}

	if len(rows) != 300 {
		t.Fatalf("Expected 300 rows, got %d", len(rows))
	}

	for i := 0; i < 300; i++ {
		expectedName := "user" + strconv.Itoa(i)
		if rows[i]["name"] != expectedName {
			t.Errorf("Row %d: expected name '%s', got '%v'", i, expectedName, rows[i]["name"])
		}
	}

	// Lookup test
	specificRow := rows[257] // Random row for validation
	fmt.Printf("Looked up row 257 -> ID: %v, Name: %v\n", specificRow["id"], specificRow["name"])
}
