// Package buildconfig parses and stages application build packaging settings.
package buildconfig

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/airlockrun/agentsdk/sourcebundle"
)

const (
	Filename        = "airlock.toml"
	maxManifestSize = 1 << 20
	maxRuntimeFiles = 256
	maxPathBytes    = 4096
)

// Config contains settings that affect application image construction.
type Config struct {
	Build Build `toml:"build"`
}

// Build controls files copied into the runtime image.
type Build struct {
	RuntimeFiles []string `toml:"runtime_files"`
}

// Selection is a validated runtime file selection.
type Selection struct {
	Paths []string
	Files []sourcebundle.File
}

// Load validates airlock.toml and resolves runtime_files against the canonical
// source bundle. A missing manifest and an empty runtime_files list select no
// files.
func Load(root string) (Selection, error) {
	manifestPath := filepath.Join(root, Filename)
	info, err := os.Lstat(manifestPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Selection{}, nil
		}
		return Selection{}, fmt.Errorf("inspect %s: %w", Filename, err)
	}
	if !info.Mode().IsRegular() {
		return Selection{}, fmt.Errorf("%s must be a regular file", Filename)
	}
	f, err := os.Open(manifestPath)
	if err != nil {
		return Selection{}, fmt.Errorf("open %s: %w", Filename, err)
	}
	defer f.Close()
	limited := io.LimitReader(f, maxManifestSize+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return Selection{}, fmt.Errorf("read %s: %w", Filename, err)
	}
	if len(data) > maxManifestSize {
		return Selection{}, fmt.Errorf("%s exceeds %d bytes", Filename, maxManifestSize)
	}
	var cfg Config
	metadata, err := toml.Decode(string(data), &cfg)
	if err != nil {
		return Selection{}, fmt.Errorf("parse %s: %w", Filename, err)
	}
	if undecoded := metadata.Undecoded(); len(undecoded) != 0 {
		keys := make([]string, len(undecoded))
		for i, key := range undecoded {
			keys[i] = key.String()
		}
		sort.Strings(keys)
		return Selection{}, fmt.Errorf("%s contains unknown keys: %s", Filename, strings.Join(keys, ", "))
	}
	if len(cfg.Build.RuntimeFiles) > maxRuntimeFiles {
		return Selection{}, fmt.Errorf("%s build.runtime_files has %d entries; maximum is %d", Filename, len(cfg.Build.RuntimeFiles), maxRuntimeFiles)
	}

	paths := make([]string, 0, len(cfg.Build.RuntimeFiles))
	for _, configured := range cfg.Build.RuntimeFiles {
		normalized, err := normalizePath(configured)
		if err != nil {
			return Selection{}, fmt.Errorf("%s build.runtime_files entry %q: %w", Filename, configured, err)
		}
		paths = append(paths, normalized)
	}
	sort.Strings(paths)
	for i, current := range paths {
		if i > 0 && (current == paths[i-1] || strings.HasPrefix(current, paths[i-1]+"/")) {
			return Selection{}, fmt.Errorf("%s build.runtime_files entries %q and %q overlap", Filename, paths[i-1], current)
		}
	}

	bundleFiles, err := sourcebundle.Files(root)
	if err != nil {
		return Selection{}, fmt.Errorf("inspect source bundle: %w", err)
	}
	byPath := make(map[string]sourcebundle.File, len(bundleFiles))
	for _, file := range bundleFiles {
		byPath[file.Path] = file
	}
	var selected []sourcebundle.File
	for _, selectedPath := range paths {
		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(selectedPath)))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return Selection{}, fmt.Errorf("%s build.runtime_files entry %q does not exist", Filename, selectedPath)
			}
			return Selection{}, fmt.Errorf("inspect runtime file %q: %w", selectedPath, err)
		}
		switch {
		case info.Mode().IsRegular():
			file, ok := byPath[selectedPath]
			if !ok {
				return Selection{}, fmt.Errorf("%s build.runtime_files entry %q is excluded from the source bundle", Filename, selectedPath)
			}
			selected = append(selected, file)
		case info.IsDir():
			included, err := sourcebundle.Included(root, selectedPath, true)
			if err != nil {
				return Selection{}, fmt.Errorf("inspect source bundle entry %q: %w", selectedPath, err)
			}
			if !included {
				return Selection{}, fmt.Errorf("%s build.runtime_files entry %q is excluded from the source bundle", Filename, selectedPath)
			}
			prefix := selectedPath + "/"
			for _, file := range bundleFiles {
				if strings.HasPrefix(file.Path, prefix) {
					selected = append(selected, file)
				}
			}
		default:
			return Selection{}, fmt.Errorf("%s build.runtime_files entry %q is not a regular file or directory", Filename, selectedPath)
		}
	}
	return Selection{Paths: paths, Files: selected}, nil
}

func normalizePath(value string) (string, error) {
	if value == "" {
		return "", errors.New("must not be empty")
	}
	if strings.Contains(value, "\\") {
		return "", errors.New("must use forward slashes")
	}
	if strings.ContainsAny(value, "*?[") {
		return "", errors.New("globs are not supported")
	}
	if path.IsAbs(value) {
		return "", errors.New("must be relative")
	}
	cleaned := path.Clean(value)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", errors.New("must stay within the application root")
	}
	if len(cleaned) > maxPathBytes {
		return "", fmt.Errorf("path exceeds %d bytes", maxPathBytes)
	}
	return cleaned, nil
}

// Stage copies a validated selection to dst with its original relative layout
// and file permissions. dst must not already exist.
func Stage(root, dst string) error {
	selection, err := Load(root)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(dst); err == nil {
		return fmt.Errorf("runtime staging destination %s already exists", dst)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("inspect runtime staging destination: %w", err)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("create runtime staging destination: %w", err)
	}
	for _, selectedPath := range selection.Paths {
		info, err := os.Stat(filepath.Join(root, filepath.FromSlash(selectedPath)))
		if err != nil {
			return err
		}
		if info.IsDir() {
			if err := os.MkdirAll(filepath.Join(dst, filepath.FromSlash(selectedPath)), info.Mode().Perm()); err != nil {
				return fmt.Errorf("create runtime directory %s: %w", selectedPath, err)
			}
		}
	}
	for _, file := range selection.Files {
		src := filepath.Join(root, filepath.FromSlash(file.Path))
		target := filepath.Join(dst, filepath.FromSlash(file.Path))
		if err := createParentDirs(root, dst, path.Dir(file.Path)); err != nil {
			return fmt.Errorf("create runtime parent for %s: %w", file.Path, err)
		}
		in, err := os.Open(src)
		if err != nil {
			return fmt.Errorf("open runtime file %s: %w", file.Path, err)
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, file.Mode.Perm())
		if err != nil {
			in.Close()
			return fmt.Errorf("create staged runtime file %s: %w", file.Path, err)
		}
		_, copyErr := io.Copy(out, in)
		inCloseErr := in.Close()
		outCloseErr := out.Close()
		if copyErr != nil {
			return fmt.Errorf("copy runtime file %s: %w", file.Path, copyErr)
		}
		if inCloseErr != nil {
			return fmt.Errorf("close runtime source %s: %w", file.Path, inCloseErr)
		}
		if outCloseErr != nil {
			return fmt.Errorf("close staged runtime file %s: %w", file.Path, outCloseErr)
		}
	}
	return nil
}

func createParentDirs(root, dst, relative string) error {
	if relative == "." {
		return nil
	}
	parts := strings.Split(relative, "/")
	for i := range parts {
		rel := path.Join(parts[:i+1]...)
		info, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			return err
		}
		target := filepath.Join(dst, filepath.FromSlash(rel))
		if err := os.Mkdir(target, info.Mode().Perm()); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		if err := os.Chmod(target, info.Mode().Perm()); err != nil {
			return err
		}
	}
	return nil
}
