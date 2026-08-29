//go:build windows

package connector

import (
	"os"
	"path/filepath"
)

// os.Root rejects Windows device names and keeps reparse-point traversal under
// the opened root handle.
func pathIsAbsolute(value string) bool { return filepath.IsAbs(value) }

func openLocalRoot(value string) (*os.Root, error) { return os.OpenRoot(value) }
