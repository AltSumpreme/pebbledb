package tests

import (
	"fmt"
	"os"
	"path/filepath"
	"pebbledb/db"
	"pebbledb/pager"
	"pebbledb/storage"
	"testing"
)

func TestWrite(t *testing.T) {
	_ = os.MkdirAll(storage.DBDir, 0775)

	table := &db.Table{
		Name: "users",
		Columns: []db.Column{
			{Name: "id", Type: db.TypeInt},
			{Name: "name", Type: db.TypeString},
		},
		Rows: []db.Row{
			{"id": 42, "name": "Reuben"},
			{"id": 43, "name": "Alice"},
			{"id": 44, "name": "Bob"},
			{"id": 45, "name": "Charlie"},
			{"id": 46, "name": "Diana"},
			{"id": 47, "name": "Eve"},
			{"id": 48, "name": "Frank"},
		},
	}

	filePath := filepath.Join(storage.DBDir, "users.db")
	file, err := os.Create(filePath)
	if err != nil {
		t.Fatalf("failed to create file: %v", err)
	}
	defer file.Close()

	page := pager.NewPage()
	for i, row := range table.Rows {
		data, err := pager.SerializeRow(row, table.Columns)
		if err != nil {
			t.Fatalf("failed to serialize row %d: %v", i, err)
		}
		_, err = page.InsertTuple(data)
		if err != nil {
			t.Fatalf("failed to insert tuple %d: %v", i, err)
		}
	}

	pageBytes := pager.SerializePage(page)
	if _, err := file.Write(pageBytes); err != nil {
		t.Fatalf("failed to write page to file: %v", err)
	}
	if err := file.Sync(); err != nil {
		t.Fatalf("failed to sync file: %v", err)
	}
}

func TestReadFromDisk(t *testing.T) {
	filePath := filepath.Join(storage.DBDir, "users.db")

	f, err := os.Open(filePath)
	if err != nil {
		t.Fatalf("Failed to open file: %v", err)
	}
	defer f.Close()

	buf := make([]byte, pager.PageSize)
	_, err = f.Read(buf)
	if err != nil {
		t.Fatalf("Failed to read file: %v", err)
	}

	page, err := pager.DeserializePage(buf)
	if err != nil {
		t.Fatalf("Failed to deserialize page: %v", err)
	}

	// Hardcoded columns
	columns := []db.Column{
		{Name: "id", Type: db.TypeInt},
		{Name: "name", Type: db.TypeString},
	}

	fmt.Println("ROWS FOUND:")
	fmt.Printf("Header.NumItems: %d\n", page.Header.NumItems)

	for i := 0; i < int(page.Header.NumItems); i++ {
		item := page.Items[i]
		if item.DeletedFlag == 0 || item.Length == 0 {
			continue
		}

		tupleData, err := page.ReadTuple(i)
		if err != nil {
			t.Errorf("Failed to read tuple %d: %v", i, err)
			continue
		}

		row, err := pager.DeserializeRow(tupleData, columns)
		if err != nil {
			t.Errorf("Failed to deserialize tuple %d: %v", i, err)
			continue
		}

		fmt.Printf("Row %d: %+v\n", i, row)
	}
}
