package storage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"pebbledb/db"
	"pebbledb/pager"
)

const DBDir = "./db"

func SaveToDisk(database *db.Database) error {
	if err := os.MkdirAll(DBDir, 0775); err != nil {
		return err
	}

	for tableName, table := range database.Tables {
		metaPath := filepath.Join(DBDir, tableName+".meta.json")
		metaFile, err := os.Create(metaPath)
		if err != nil {
			return fmt.Errorf("create schema file for %s: %w", tableName, err)
		}
		if err := json.NewEncoder(metaFile).Encode(table.Columns); err != nil {
			return fmt.Errorf("encode schema for %s: %w", tableName, err)
		}
		metaFile.Close()
		dataPath := filepath.Join(DBDir, tableName+".db")
		dataFile, err := os.Create(dataPath)
		if err != nil {
			return fmt.Errorf("create data file for %s: %w", tableName, err)
		}

		page := pager.NewPage()
		for _, row := range table.Rows {
			fmt.Printf("Serializing row: %v\n", row)
			serialized, err := pager.SerializeRow(row, table.Columns)
			fmt.Printf("Serialized row: %v\n", serialized)
			if err != nil {
				return fmt.Errorf("serialize row: %w", err)
			}
			if _, err := page.InsertTuple(serialized); err != nil {
				return fmt.Errorf("insert tuple: %w", err)
			}
		}

		if _, err := dataFile.Write(pager.SerializePage(page)); err != nil {
			return fmt.Errorf("write page for %s: %w", tableName, err)
		}
		dataFile.Close()
	}

	return nil
}

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
func LoadFromDisk() (*db.Database, error) {
	files, err := os.ReadDir(DBDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read directory %s: %w", DBDir, err)
	}
	database := db.NewDatabase()
	for _, file := range files {
		if file.IsDir() {
			continue
		}
		filePath := fmt.Sprintf(DBDir + "/" + file.Name())
		f, err := os.Open(filePath)
		if err != nil {
			return nil, fmt.Errorf("failed to open file %s: %w", filePath, err)
		}

		buf := make([]byte, pager.PageSize)
		_, err = f.Read(buf)
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("failed to read file %s: %w", filePath, err)
		}

		page, err := pager.DeserializePage(buf)

		if err != nil {
			return nil, fmt.Errorf("failed to deserialize page from file %s: %w", filePath, err)
		}
		if filepath.Ext(filePath) != ".db" {
			continue
		}
		tableName := file.Name()
		tableName = tableName[:len(tableName)-3]
		columnDef, err := LoadSchemaFromDisk(tableName)
		if err != nil {
			return nil, fmt.Errorf("table %s does not exist in database: %w", tableName, err)
		}
		table := &db.Table{
			Name:    tableName,
			Columns: columnDef,
			Rows:    []db.Row{},
		}
		for i := 0; i < int(page.Header.NumItems); i++ {
			items := page.Items[i]
			if items.DeletedFlag == 0 || items.Length == 0 {
				continue
			}

			tupleData, err := page.ReadTuple(i)

			if err != nil {
				return nil, fmt.Errorf("failed to read tuple from page: %w", err)
			}
			row, err := pager.DeserializeRow(tupleData, table.Columns)
			if err != nil {
				return nil, fmt.Errorf("failed to deserialize row: %w", err)
			}
			fmt.Printf("Deserialized row: %v\n", row)
			table.Rows = append(table.Rows, row)

		}
		database.Tables[tableName] = table

	}
	return database, nil

}
