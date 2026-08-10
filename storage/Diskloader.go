package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"pebbledb/db"
	"pebbledb/pager"
	"sort"
	"strconv"
	"strings"
)

func LoadFromDisk(dataDir string) (*db.Database, error) {
	if dataDir == "" {
		return nil, fmt.Errorf("data directory cannot be empty")
	}
	files, err := os.ReadDir(dataDir)
	if err != nil {
		if os.IsNotExist(err) {
			return db.NewDatabase(), nil
		}
		return nil, fmt.Errorf("failed to read directory %s: %w", dataDir, err)
	}
	tablesMap := map[string][]pageFile{}
	tableNames := map[string]struct{}{}
	database := db.NewDatabase()
	for _, file := range files {
		if file.IsDir() {
			continue
		}
		if strings.HasSuffix(file.Name(), ".meta.json") {
			tableName := db.NormalizeIdentifier(strings.TrimSuffix(file.Name(), ".meta.json"))
			if tableName != "" {
				tableNames[tableName] = struct{}{}
			}
			continue
		}
		if filepath.Ext(file.Name()) == ".db" {
			tableName, pageIndex, err := parsePageFileName(file.Name())
			if err != nil {
				return nil, err
			}
			tableName = db.NormalizeIdentifier(tableName)
			tableNames[tableName] = struct{}{}
			tablesMap[tableName] = append(tablesMap[tableName], pageFile{
				name:  file.Name(),
				index: pageIndex,
			})

		}
	}

	orderedTableNames := make([]string, 0, len(tableNames))
	for tableName := range tableNames {
		orderedTableNames = append(orderedTableNames, tableName)
	}
	sort.Strings(orderedTableNames)

	for _, tableName := range orderedTableNames {
		pageFile := tablesMap[tableName]
		sort.Slice(pageFile, func(i, j int) bool {
			return pageFile[i].index < pageFile[j].index
		})
		columnDef, err := LoadSchemaFromDisk(dataDir, tableName)
		if err != nil {
			return nil, fmt.Errorf("failed to load schema for table %s: %w", tableName, err)
		}
		table := db.Table{
			Name:    tableName,
			Columns: columnDef,
			Rows:    []db.Row{},
		}

		for _, page := range pageFile {
			path := filepath.Join(dataDir, page.name)
			buf, err := os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("failed to read file %s: %w", path, err)
			}
			if len(buf) != pager.PageSize {
				return nil, fmt.Errorf("invalid page size for %s: got %d bytes, expected %d", path, len(buf), pager.PageSize)
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

type pageFile struct {
	name  string
	index int
}

func parsePageFileName(name string) (string, int, error) {
	baseFile := strings.TrimSuffix(name, filepath.Ext(name))
	underscoreIndex := strings.LastIndex(baseFile, "_")
	if underscoreIndex == -1 {
		return "", 0, fmt.Errorf("invalid file name %s, expected format <table>_<page>.db", name)
	}
	tableName := baseFile[:underscoreIndex]
	if tableName == "" {
		return "", 0, fmt.Errorf("invalid file name %s: missing table name", name)
	}
	pageIndex, err := strconv.Atoi(baseFile[underscoreIndex+1:])
	if err != nil || pageIndex < 0 {
		return "", 0, fmt.Errorf("invalid page index in file name %s", name)
	}
	return tableName, pageIndex, nil
}
