//go:build unix

package lsm

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func acquireDirectoryLock(directory string) (*os.File, error) {
	file, err := os.OpenFile(filepath.Join(directory, "LOCK"), os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, fmt.Errorf("lsm: open directory lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrLocked
		}
		return nil, fmt.Errorf("lsm: acquire directory lock: %w", err)
	}
	return file, nil
}

func releaseDirectoryLock(file *os.File) error {
	if file == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	closeErr := file.Close()
	return errors.Join(unlockErr, closeErr)
}
