package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"pebbledb/db"
	"pebbledb/pager"
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

	for tableName, pageFile := range tablesMap {
		columnDef, err := LoadSchemaFromDisk(tableName)
		if err != nil {
			return nil, fmt.Errorf("failed to load schema for table %s: %w", tableName, err)
		}
		table := db.Table{
			Name:    tableName,
			Columns: columnDef,
			Rows:    []db.Row{},
		}

		for _, files := range pageFile {
			path := filepath.Join(DBDir, files)
			file, err := os.Open(path)
			if err != nil {
				return nil, fmt.Errorf("failed to open file %s: %w", path, err)
			}
			buf := make([]byte, pager.PageSize)
			_, err = file.Read(buf)
			if err != nil {
				return nil, fmt.Errorf("failed to read file %s: %w", path, err)
			}

			page, err := pager.DeserializePage(buf)
			if err != nil {
				return nil, fmt.Errorf("failed to deserialize page from file %s: %w", path, err)
			}
			for i := 0; i < pager.MaxItemsPerPage; i++ {
				if page.Items[i].DeletedFlag == 0 || page.Items[i].Length == 0 {
					continue
				}

				tupleData, err := page.ReadTuple(i)
				if err != nil {
					return nil, fmt.Errorf("failed to read tuple %d from page: %w", i, err)
				}
				deserialized, err := pager.DeserializeRow(tupleData, columnDef)
				if err != nil {
					return nil, fmt.Errorf("failed to deserialize row from tuple %d: %w", i, err)
				}
				table.Rows = append(table.Rows, deserialized)

			}

		}
		database.Tables[tableName] = &table

	}
	return database, nil
}
