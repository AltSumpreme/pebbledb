package storage

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"pebbledb/db"
	"pebbledb/pager"
	"strings"
)

const DefaultDataDir = ".pebbledb"

func SaveToDisk(database *db.Database, dataDir string) error {
	if dataDir == "" {
		return fmt.Errorf("data directory cannot be empty")
	}
	if err := os.MkdirAll(dataDir, 0775); err != nil {
		return err
	}

	expectedFiles := make(map[string]struct{})
	for tableName, table := range database.Tables {
		metaName := tableName + ".meta.json"
		expectedFiles[metaName] = struct{}{}
		metaPath := filepath.Join(dataDir, metaName)
		if err := writeJSONAtomic(metaPath, table.Columns); err != nil {
			return fmt.Errorf("encode schema for %s: %w", tableName, err)
		}

		pageIndex := 0

		currentPage := pager.NewPage()
		for _, row := range table.Rows {
			serialized, err := pager.SerializeRow(row, table.Columns)
			if err != nil {
				return fmt.Errorf("serialize row: %w", err)
			}
			if slot, err := currentPage.InsertTuple(serialized); err != nil {
				if slot == -2 {
					pageFileName := fmt.Sprintf("%s_%d.db", tableName, pageIndex)
					expectedFiles[pageFileName] = struct{}{}
					if err := writePageAtomic(filepath.Join(dataDir, pageFileName), currentPage); err != nil {
						return fmt.Errorf("write page %d for %s: %w", pageIndex, tableName, err)
					}
				} else {
					return fmt.Errorf("insert tuple: %w", err)
				}
				pageIndex++

				currentPage = pager.NewPage()
				if _, err := currentPage.InsertTuple(serialized); err != nil {
					return fmt.Errorf("insert tuple in new page: %w", err)
				}
			}
		}

		if currentPage.Header.NumItems > 0 {
			pageFileName := fmt.Sprintf("%s_%d.db", tableName, pageIndex)
			expectedFiles[pageFileName] = struct{}{}
			if err := writePageAtomic(filepath.Join(dataDir, pageFileName), currentPage); err != nil {
				return fmt.Errorf("write final page for %s: %w", tableName, err)
			}
		}
	}

	if err := removeStaleFiles(dataDir, expectedFiles); err != nil {
		return err
	}
	if err := syncDir(dataDir); err != nil {
		return err
	}
	return nil
}

func writeJSONAtomic(path string, value interface{}) error {
	return writeFileAtomic(path, func(w io.Writer) error {
		return json.NewEncoder(w).Encode(value)
	})
}

func writePageAtomic(path string, page *pager.Page) error {
	pageBytes := pager.SerializePage(page)
	return writeFileAtomic(path, func(w io.Writer) error {
		_, err := w.Write(pageBytes)
		return err
	})
}

func writeFileAtomic(path string, write func(io.Writer) error) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := write(tmp); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return syncDir(dir)
}

func removeStaleFiles(dataDir string, expected map[string]struct{}) error {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return fmt.Errorf("read data directory %s: %w", dataDir, err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !isDataFile(name) {
			continue
		}
		if _, ok := expected[name]; ok {
			continue
		}
		if err := os.Remove(filepath.Join(dataDir, name)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove stale file %s: %w", name, err)
		}
	}
	return nil
}

func isDataFile(name string) bool {
	return strings.HasSuffix(name, ".db") || strings.HasSuffix(name, ".meta.json")
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
