//go:build !windows

package connector

import (
	"os"
	"path/filepath"
)

func pathIsAbsolute(value string) bool { return filepath.IsAbs(value) }

func openLocalRoot(value string) (*os.Root, error) { return os.OpenRoot(value) }
