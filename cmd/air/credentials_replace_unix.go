//go:build !windows

package main

import "os"

func replaceCredentialFile(source, target string) error {
	return os.Rename(source, target)
}

func syncCredentialDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func credentialFileModeSecure(path string) bool {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return true
	}
	return err == nil && info.Mode().Perm() == 0o600
}
