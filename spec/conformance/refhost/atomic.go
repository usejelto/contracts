package main

import (
	"os"
	"path/filepath"
)

func atomicWrite(path string, data []byte, durable bool) bool {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return false
	}
	temporary := path + ".tmp"
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return false
	}
	defer os.Remove(temporary)
	if _, err = file.Write(data); err == nil && durable {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		return false
	}
	return os.Rename(temporary, path) == nil
}
