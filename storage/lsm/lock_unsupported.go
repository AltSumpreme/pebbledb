//go:build !unix

package lsm

import (
	"fmt"
	"os"
)

func acquireDirectoryLock(string) (*os.File, error) {
	return nil, fmt.Errorf("lsm: directory locking is unsupported on this operating system")
}

func releaseDirectoryLock(file *os.File) error {
	if file == nil {
		return nil
	}
	return file.Close()
}
