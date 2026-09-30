//go:build windows

package main

import (
	"path/filepath"

	"golang.org/x/sys/windows"
)

func replaceCredentialFile(source, target string) error {
	sourcePtr, err := windows.UTF16PtrFromString(filepath.Clean(source))
	if err != nil {
		return err
	}
	targetPtr, err := windows.UTF16PtrFromString(filepath.Clean(target))
	if err != nil {
		return err
	}
	return windows.MoveFileEx(sourcePtr, targetPtr, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

func syncCredentialDirectory(string) error {
	return nil
}

func credentialFileModeSecure(string) bool {
	return true
}
