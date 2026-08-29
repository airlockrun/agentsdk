//go:build darwin

package connector

import (
	"errors"
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
	if mode != ServiceUser {
		return "", errors.New("connector: macOS system services are unsupported; use connector.ServiceUser")
	}
	config, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(config, "Airlock", "Connectors", kind), nil
}

func prepareStateDirectory(path string, mode ServiceMode, _ Operations) error {
	if mode != ServiceUser {
		return errors.New("connector: macOS system services are unsupported; use connector.ServiceUser")
	}
	return ensurePrivateDirectory(path)
}
