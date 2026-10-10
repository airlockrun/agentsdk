package localruntime

import (
	"errors"
	"os"
	"path/filepath"
)

func atomicCacheFile(dir, name string, data []byte, mode os.FileMode) error {
	file, err := os.CreateTemp(dir, ".publish-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, err = file.Write(data)
	err = errors.Join(err, file.Chmod(mode), file.Sync(), file.Close())
	if err != nil {
		return err
	}
	if err := os.Rename(file.Name(), filepath.Join(dir, name)); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
