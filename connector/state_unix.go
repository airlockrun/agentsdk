//go:build !windows && !darwin

package connector

import (
	"os"
	"path/filepath"
)

func protectBytes(value []byte, _ bool) ([]byte, error) {
	return append([]byte(nil), value...), nil
}

func unprotectBytes(value []byte, _ bool) ([]byte, error) {
	return append([]byte(nil), value...), nil
}

func replaceFile(from, to string) error { return os.Rename(from, to) }

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func defaultStateDir(kind string, mode ServiceMode) (string, error) {
	if mode == ServiceSystem {
		return filepath.Join("/var/lib/airlock/connectors", kind), nil
	}
	config, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(config, "airlock", "connectors", kind), nil
}

func prepareStateDirectory(path string, _ ServiceMode, _ Operations) error {
	return ensurePrivateDirectory(path)
}
