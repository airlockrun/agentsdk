//go:build windows

package connector

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"
)

type dataBlob struct {
	size uint32
	data *byte
}

var (
	crypt32            = syscall.NewLazyDLL("crypt32.dll")
	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	cryptProtectData   = crypt32.NewProc("CryptProtectData")
	cryptUnprotectData = crypt32.NewProc("CryptUnprotectData")
	localFree          = kernel32.NewProc("LocalFree")
	moveFileEx         = kernel32.NewProc("MoveFileExW")
)

func protectBytes(value []byte, machine bool) ([]byte, error) {
	flags := uintptr(0x1)
	if machine {
		flags |= 0x4
	}
	return cryptData(cryptProtectData, value, flags)
}

func replaceFile(from, to string) error {
	fromPtr, err := syscall.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	toPtr, err := syscall.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	result, _, callErr := moveFileEx.Call(uintptr(unsafe.Pointer(fromPtr)), uintptr(unsafe.Pointer(toPtr)), 0x1|0x8)
	if result == 0 {
		return fmt.Errorf("MoveFileExW: %w", callErr)
	}
	return nil
}

func syncDirectory(string) error { return nil }

func unprotectBytes(value []byte, _ bool) ([]byte, error) {
	return cryptData(cryptUnprotectData, value, 0x1)
}

func cryptData(proc *syscall.LazyProc, value []byte, flags uintptr) ([]byte, error) {
	var input dataBlob
	if len(value) > 0 {
		input = dataBlob{size: uint32(len(value)), data: &value[0]}
	}
	var output dataBlob
	result, _, callErr := proc.Call(uintptr(unsafe.Pointer(&input)), 0, 0, 0, 0, flags, uintptr(unsafe.Pointer(&output)))
	if result == 0 {
		return nil, fmt.Errorf("DPAPI: %w", callErr)
	}
	defer localFree.Call(uintptr(unsafe.Pointer(output.data)))
	return append([]byte(nil), unsafe.Slice(output.data, output.size)...), nil
}

func defaultStateDir(kind string, mode ServiceMode) (string, error) {
	if mode == ServiceSystem {
		root := os.Getenv("ProgramData")
		if root == "" {
			return "", errors.New("connector: ProgramData is required for a Windows system service")
		}
		return filepath.Join(root, "Airlock", "Connectors", kind), nil
	}
	config, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(config, "Airlock", "Connectors", kind), nil
}

func prepareStateDirectory(path string, mode ServiceMode, operations Operations) error {
	if err := ensurePrivateDirectory(path); err != nil {
		return err
	}
	if mode != ServiceSystem {
		return nil
	}
	return setSystemStateACL(path, "", operations)
}

func setSystemStateACL(path, serviceAccount string, operations Operations) error {
	grants := []string{path, "/inheritance:r", "/grant:r", `SYSTEM:(OI)(CI)F`, `BUILTIN\Administrators:(OI)(CI)F`}
	if serviceAccount != "" {
		grants = append(grants, serviceAccount+`:(OI)(CI)F`)
	}
	_, err := operations.Execute(context.Background(), "icacls.exe", grants...)
	return err
}
