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
		// write schema file (close immediately)
		metaPath := filepath.Join(DBDir, tableName+".meta.json")
		metaFile, err := os.Create(metaPath)
		if err != nil {
			return fmt.Errorf("create schema file for %s: %w", tableName, err)
		}
		if err := json.NewEncoder(metaFile).Encode(table.Columns); err != nil {
			metaFile.Close()
			return fmt.Errorf("encode schema for %s: %w", tableName, err)
		}
		metaFile.Close()

		pageIndex := 0
		currentPage := pager.PageInit()

		// iterate all pages of the table and copy tuples into new page files
		for _, pageID := range table.PageNo {
			page, err := table.PageManager.GetPage(pageID)
			if err != nil {
				return fmt.Errorf("get page %d for %s: %w", pageID, tableName, err)
			}
			tuples, err := page.GetAllTuples()
			if err != nil {
				return fmt.Errorf("get all tuples from page %d for %s: %w", pageID, tableName, err)
			}

			for _, tupleBytes := range tuples {
				// insert into current in-memory page
				slot, err := currentPage.InsertTuple(tupleBytes)
				if err != nil {
					// case when current page is full, flush to disk and create new page
					if slot == -2 {
						pageBytes := pager.SerializePage(currentPage)
						pageFileName := fmt.Sprintf("%s_%d.db", tableName, pageIndex)
						filePath := filepath.Join(DBDir, pageFileName)
						if err := os.WriteFile(filePath, pageBytes, 0664); err != nil {
							return fmt.Errorf("write page %d for %s: %w", pageIndex, tableName, err)
						}
						pageIndex++
						currentPage = pager.PageInit()
						// insert into fresh page
						if _, err := currentPage.InsertTuple(tupleBytes); err != nil {
							return fmt.Errorf("insert tuple in new page: %w", err)
						}
					} else {
						return fmt.Errorf("insert tuple: %w", err)
					}
				}
			}
		}

		// flush remaining tuples in currentPage if any
		if currentPage.Header.NumItems > 0 {
			pageBytes := pager.SerializePage(currentPage)
			pageFileName := fmt.Sprintf("%s_%d.db", tableName, pageIndex)
			filePath := filepath.Join(DBDir, pageFileName)
			if err := os.WriteFile(filePath, pageBytes, 0664); err != nil {
				return fmt.Errorf("write final page for %s: %w", tableName, err)
			}
		}
	}

	return nil
}
