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

		page_index := 0

		currentPage := pager.NewPage()
		for _, row := range table.Rows {
			fmt.Printf("Serializing row: %v\n", row)
			serialized, err := pager.SerializeRow(row, table.Columns)
			fmt.Printf("Serialized row: %v\n", serialized)
			if err != nil {
				return fmt.Errorf("serialize row: %w", err)
			}
			if slot, err := currentPage.InsertTuple(serialized); err != nil {
				if slot == -2 {
					pageBytes := pager.SerializePage(currentPage)
					pageFileName := fmt.Sprintf("%s_%d.db", tableName, page_index)
					filePath := filepath.Join(DBDir, pageFileName)
					if err := os.WriteFile(filePath, pageBytes, 0664); err != nil {
						return fmt.Errorf("write page %d for %s: %w", page_index, tableName, err)
					}
				} else {
					return fmt.Errorf("insert tuple: %w", err)
				}
				page_index++

				currentPage = pager.NewPage()
				if _, err := currentPage.InsertTuple(serialized); err != nil {
					return fmt.Errorf("insert tuple in new page: %w", err)
				}
			}
			if len(currentPage.Data) > 0 {
				pageBytes := pager.SerializePage(currentPage)
				pageFileName := fmt.Sprintf("%s_%d.db", tableName, page_index)
				filePath := filepath.Join(DBDir, pageFileName)
				if err := os.WriteFile(filePath, pageBytes, 0664); err != nil {
					return fmt.Errorf("write final page for %s: %w", tableName, err)
				}
			}

		}

	}
	return nil
}
