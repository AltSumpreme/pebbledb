package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"pebbledb/db"
	"pebbledb/pagemanager"
	"strings"
)

func LoadFromDisk() (*db.Database, error) {
	files, err := os.ReadDir(DBDir)
	var tableName string
	if err != nil {
		return nil, fmt.Errorf("failed to read directory %s: %w", DBDir, err)
	}
	tablesMap := map[string][]string{}
	database := db.NewDatabase()
	for _, file := range files {
		if file.IsDir() {
			continue
		}
		if filepath.Ext(file.Name()) == ".db" {
			base_file := strings.TrimSuffix(file.Name(), filepath.Ext(file.Name()))
			underscoreIndex := strings.LastIndex(base_file, "_")
			if underscoreIndex == -1 {
				return nil, fmt.Errorf("invalid file name %s, expected format <table>_<page>_<no>.db", file.Name())
			}
			tableName = base_file[:underscoreIndex]
			tablesMap[tableName] = append(tablesMap[tableName], file.Name())

		}
	}

	for tableName, pageFiles := range tablesMap {
		columnDef, err := LoadSchemaFromDisk(tableName)
		if err != nil {
			return nil, fmt.Errorf("failed to load schema for table %s: %w", tableName, err)
		}

		// Build pageFiles for page manager   index -> filepath
		pageFileMap := make(map[int]string)
		for idx, filename := range pageFiles {
			pagePath := filepath.Join(DBDir, filename)
			pageFileMap[idx] = pagePath
		}

		pm := pagemanager.NewPageManager(pageFileMap, 0)

		table := db.Table{
			Name:        tableName,
			Columns:     columnDef,
			PageManager: pm,
			PageNo:      make([]int, 0, len(pageFiles)),
		}

		for idx := range pageFileMap {
			table.PageNo = append(table.PageNo, idx)
		}

		database.Tables[tableName] = &table

	}
	return database, nil
}
