package scaffold

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

//go:embed skills
var skillFiles embed.FS

// SkillsDigest identifies the exact embedded skill bundle.
func SkillsDigest() string {
	manifest, err := skillFiles.ReadFile("skills/manifest.json")
	if err != nil {
		panic(fmt.Sprintf("scaffold: read embedded skill manifest: %v", err))
	}
	sum := sha256.Sum256(manifest)
	return hex.EncodeToString(sum[:])
}

// InstallSkills replaces dir with the version-matched embedded skill bundle.
func InstallSkills(dir string) error {
	parent := filepath.Dir(dir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create skill parent directory: %w", err)
	}
	tmp, err := os.MkdirTemp(parent, ".skills-install-")
	if err != nil {
		return fmt.Errorf("create temporary skill directory: %w", err)
	}
	defer os.RemoveAll(tmp)

	err = fs.WalkDir(skillFiles, "skills", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(path, "skills/")
		if rel == "skills" || rel == "" {
			return nil
		}
		dst := filepath.Join(tmp, filepath.FromSlash(rel))
		if entry.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		data, err := skillFiles.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dst, data, 0o644)
	})
	if err != nil {
		return fmt.Errorf("materialize embedded skills: %w", err)
	}

	backup := dir + ".old"
	if err := os.RemoveAll(backup); err != nil {
		return fmt.Errorf("remove old skill backup: %w", err)
	}
	if _, err := os.Stat(dir); err == nil {
		if err := os.Rename(dir, backup); err != nil {
			return fmt.Errorf("back up installed skills: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect installed skills: %w", err)
	}
	if err := os.Rename(tmp, dir); err != nil {
		_ = os.Rename(backup, dir)
		return fmt.Errorf("install skills: %w", err)
	}
	if err := os.RemoveAll(backup); err != nil {
		return fmt.Errorf("remove skill backup: %w", err)
	}
	return nil
}
