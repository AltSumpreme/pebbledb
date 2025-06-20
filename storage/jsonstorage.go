package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"pebbledb/db"
)

const Path = "./db.json"

func SaveToDisk(database *db.Database) error {
	file, err := os.Create(Path)

	if err != nil {
		return err
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	return encoder.Encode(database)
}
func LoadFromDisk() (*db.Database, error) {
	data, err := os.ReadFile(Path)
	if err != nil {
		return nil, fmt.Errorf("failed to read storage file: %w", err)
	}

	if len(data) == 0 {
		fmt.Println("Storage file is empty. Initializing fresh database.")
		return db.NewDatabase(), nil
	}

	var dbState db.Database
	if err := json.Unmarshal(data, &dbState); err != nil {
		return nil, fmt.Errorf("failed to parse JSON: %w", err)
	}
	if dbState.Tables == nil {
		fmt.Println("Warning: No tables loaded from disk.")
	}

	if len(dbState.Tables) == 0 {
		fmt.Println("Warning: No tables loaded from disk.")
	}
	for _, table := range dbState.Tables {
		normalizeRows(table)
	}

	return &dbState, nil
}

func normalizeRows(table *db.Table) {
	for i, row := range table.Rows {
		newRow := make(db.Row)
		for _, col := range table.Columns {
			val := row[col.Name]
			switch col.Type {
			case db.TypeInt:
				if f, ok := val.(float64); ok {
					newRow[col.Name] = int(f)
				} else {
					newRow[col.Name] = val
				}
			default:
				newRow[col.Name] = val
			}
		}
		table.Rows[i] = newRow
	}
}
