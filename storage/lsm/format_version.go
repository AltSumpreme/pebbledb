package lsm

import (
	"fmt"
	"os"
	"path/filepath"
)

const currentFormatVersion = "pebbledb-storage-v1\n"

func ensureFormatVersion(directory string) error {
	path := filepath.Join(directory, "FORMAT")
	value, err := os.ReadFile(path)
	if err == nil {
		if string(value) != currentFormatVersion {
			return fmt.Errorf("lsm: unsupported storage format %q", string(value))
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return fmt.Errorf("lsm: read storage format: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("lsm: create storage format: %w", err)
	}
	if err := writeAll(file, []byte(currentFormatVersion)); err != nil {
		_ = file.Close()
		return fmt.Errorf("lsm: write storage format: %w", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return fmt.Errorf("lsm: sync storage format: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("lsm: close storage format: %w", err)
	}
	if err := syncDirectory(directory); err != nil {
		return fmt.Errorf("lsm: sync storage format directory: %w", err)
	}
	return nil
}
