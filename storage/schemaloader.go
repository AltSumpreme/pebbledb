package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"pebbledb/db"
)

func LoadSchemaFromDisk(tableName string) ([]db.Column, error) {
	metafile := filepath.Join(DBDir, tableName+".meta.json")
	if _, err := os.Stat(metafile); os.IsNotExist(err) {
		return nil, fmt.Errorf("schema file for table %s does not exist", tableName)
	}
	file, err := os.Open(metafile)
	if err != nil {
		return nil, fmt.Errorf("failed to open schema file for table %s: %w", tableName, err)
	}
	defer file.Close()

	var columnDef []db.Column
	if err := json.NewDecoder(file).Decode(&columnDef); err != nil {
		return nil, fmt.Errorf("failed to decode schema for table %s: %w", tableName, err)
	}
	return columnDef, nil
}
