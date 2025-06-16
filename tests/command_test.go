package tests

import (
	"pebbledb/db"

	"testing"
)

func TestCreateInsertSelectDrop(t *testing.T) {
	database := db.NewDatabase()
	err := database.CreateTable("users", []db.Column{
		{Name: "id", Type: "INT"},
		{Name: "name", Type: db.TypeString},
		{Name: "email", Type: db.TypeString},
	})
	if err != nil {
		t.Errorf("Failed to create table: %v", err)
		return
	}
	table, err := database.GetTable("users")

	if err != nil {
		t.Errorf("Failed to get table: %v", err)
		return
	}
	if table.Name != "users" {
		t.Errorf("Expected table name 'users', got '%s'", table.Name)
		return
	}
	err = database.InsertValue("users", []string{"2", "Alice", "alice@example.com"})

	if err != nil {
		t.Errorf("Failed to insert value: %v", err)
		return
	}
	rows, err := database.SelectAll("users")
	if err != nil {
		t.Errorf("Failed to select all values: %v", err)
		return
	}
	t.Logf("Row content: %+v", rows[0])
	if len(rows) != 1 {
		t.Errorf("Expected 1 row, got %d", len(rows))
		return
	}
	if rows[0]["name"] != "Alice" {
		t.Errorf("Expected name 'Alice', got '%v'", rows[0]["name"])
		return
	}
	err = database.DropTable("users")
	if err != nil {
		t.Errorf("Failed to drop table: %v", err)
		return
	}

}
