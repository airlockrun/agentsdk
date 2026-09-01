//go:build windows

package connector

import (
	"fmt"
	"syscall"
	"unsafe"
)

var moveFileEx = syscall.NewLazyDLL("kernel32.dll").NewProc("MoveFileExW")

func replaceFile(from, to string) error {
	fromPointer, err := syscall.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	toPointer, err := syscall.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	result, _, callErr := moveFileEx.Call(uintptr(unsafe.Pointer(fromPointer)), uintptr(unsafe.Pointer(toPointer)), 0x1|0x8)
	if result == 0 {
		return fmt.Errorf("MoveFileExW: %w", callErr)
	}
	return nil
}

func syncDirectory(string) error { return nil }
