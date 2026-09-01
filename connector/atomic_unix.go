//go:build !windows

package connector

import "os"

func replaceFile(from, to string) error { return os.Rename(from, to) }

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
