// Package sourcebundle defines the source tree Airlock synchronizes with local
// workspaces. Hashing and archive creation use the same deterministic file set.
package sourcebundle

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// StatePrefix identifies source-state hashes on the wire and in agent.toml.
const StatePrefix = "sha256:"

type fileEntry struct {
	path string
	mode fs.FileMode
	size int64
}

type ignoreRule struct {
	base    string
	negated bool
	re      *regexp.Regexp
}

// Digest returns the deterministic content state for root.
func Digest(root string) (string, error) {
	entries, err := entries(root)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	for _, entry := range entries {
		writeDigestHeader(h, entry.path, entry.mode&0o111 != 0, entry.size)
		f, err := os.Open(filepath.Join(root, filepath.FromSlash(entry.path)))
		if err != nil {
			return "", fmt.Errorf("open %s: %w", entry.path, err)
		}
		_, copyErr := io.Copy(h, f)
		closeErr := f.Close()
		if copyErr != nil {
			return "", fmt.Errorf("hash %s: %w", entry.path, copyErr)
		}
		if closeErr != nil {
			return "", fmt.Errorf("close %s: %w", entry.path, closeErr)
		}
	}
	return StatePrefix + hex.EncodeToString(h.Sum(nil)), nil
}

func writeDigestHeader(w io.Writer, path string, executable bool, size int64) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(path)))
	_, _ = w.Write(length[:])
	_, _ = io.WriteString(w, path)
	if executable {
		_, _ = w.Write([]byte{1})
	} else {
		_, _ = w.Write([]byte{0})
	}
	binary.BigEndian.PutUint64(length[:], uint64(size))
	_, _ = w.Write(length[:])
}

// WriteArchive writes root as a deterministic tar.gz source archive and
// returns the state represented by that archive.
func WriteArchive(w io.Writer, root string) (string, error) {
	state, err := Digest(root)
	if err != nil {
		return "", err
	}
	entries, err := entries(root)
	if err != nil {
		return "", err
	}
	gz := gzip.NewWriter(w)
	gz.Header.ModTime = time.Unix(0, 0)
	tw := tar.NewWriter(gz)
	for _, entry := range entries {
		hdr := &tar.Header{
			Name:    entry.path,
			Mode:    int64(entry.mode.Perm()),
			Size:    entry.size,
			ModTime: time.Unix(0, 0),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			_ = tw.Close()
			_ = gz.Close()
			return "", fmt.Errorf("write header %s: %w", entry.path, err)
		}
		f, err := os.Open(filepath.Join(root, filepath.FromSlash(entry.path)))
		if err != nil {
			_ = tw.Close()
			_ = gz.Close()
			return "", fmt.Errorf("open %s: %w", entry.path, err)
		}
		_, copyErr := io.Copy(tw, f)
		closeErr := f.Close()
		if copyErr != nil {
			_ = tw.Close()
			_ = gz.Close()
			return "", fmt.Errorf("archive %s: %w", entry.path, copyErr)
		}
		if closeErr != nil {
			_ = tw.Close()
			_ = gz.Close()
			return "", fmt.Errorf("close %s: %w", entry.path, closeErr)
		}
	}
	if err := tw.Close(); err != nil {
		_ = gz.Close()
		return "", fmt.Errorf("close tar: %w", err)
	}
	if err := gz.Close(); err != nil {
		return "", fmt.Errorf("close gzip: %w", err)
	}
	return state, nil
}

// ExtractArchive extracts a source archive into an empty or existing directory.
// Only regular files with safe relative paths are accepted.
func ExtractArchive(r io.Reader, dst string) error {
	_, err := ExtractArchiveState(r, dst)
	return err
}

// ExtractArchiveState extracts a canonical source archive and returns the state
// represented by its paths, executable bits, sizes, and contents. Computing the
// state from archive metadata keeps verification portable to filesystems that
// cannot represent Unix executable bits, including Windows.
func ExtractArchiveState(r io.Reader, dst string) (string, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return "", fmt.Errorf("open gzip: %w", err)
	}
	defer gz.Close()
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return "", err
	}
	tr := tar.NewReader(gz)
	h := sha256.New()
	previous := ""
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return StatePrefix + hex.EncodeToString(h.Sum(nil)), nil
		}
		if err != nil {
			return "", fmt.Errorf("read tar: %w", err)
		}
		rel := filepath.ToSlash(filepath.Clean(hdr.Name))
		if rel == "." || rel == "" || strings.HasPrefix(rel, "/") || rel == ".." || strings.HasPrefix(rel, "../") || fixedExcluded(rel) {
			return "", fmt.Errorf("source archive contains invalid path %q", hdr.Name)
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			return "", fmt.Errorf("source archive contains unsupported entry %s", rel)
		}
		if previous != "" && rel <= previous {
			return "", fmt.Errorf("source archive entries are not in canonical order: %q after %q", rel, previous)
		}
		previous = rel
		writeDigestHeader(h, rel, fs.FileMode(hdr.Mode)&0o111 != 0, hdr.Size)
		path := filepath.Join(dst, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return "", err
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fs.FileMode(hdr.Mode)&0o777)
		if err != nil {
			return "", fmt.Errorf("create %s: %w", rel, err)
		}
		_, copyErr := io.Copy(io.MultiWriter(f, h), tr)
		closeErr := f.Close()
		if copyErr != nil {
			return "", fmt.Errorf("extract %s: %w", rel, copyErr)
		}
		if closeErr != nil {
			return "", fmt.Errorf("close %s: %w", rel, closeErr)
		}
	}
}

// Mirror replaces dst's synchronized source files with src while preserving
// excluded and ignored local state such as .git and .airlock.
func Mirror(src, dst string) error {
	dstEntries, err := entries(dst)
	if err != nil {
		return err
	}
	for _, entry := range dstEntries {
		if err := os.Remove(filepath.Join(dst, filepath.FromSlash(entry.path))); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", entry.path, err)
		}
	}
	srcEntries, err := entries(src)
	if err != nil {
		return err
	}
	for _, entry := range srcEntries {
		srcPath := filepath.Join(src, filepath.FromSlash(entry.path))
		dstPath := filepath.Join(dst, filepath.FromSlash(entry.path))
		if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
			return err
		}
		in, err := os.Open(srcPath)
		if err != nil {
			return fmt.Errorf("open %s: %w", entry.path, err)
		}
		out, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, entry.mode.Perm())
		if err != nil {
			in.Close()
			return fmt.Errorf("create %s: %w", entry.path, err)
		}
		_, copyErr := io.Copy(out, in)
		inCloseErr := in.Close()
		outCloseErr := out.Close()
		if copyErr != nil {
			return fmt.Errorf("copy %s: %w", entry.path, copyErr)
		}
		if inCloseErr != nil {
			return fmt.Errorf("close source %s: %w", entry.path, inCloseErr)
		}
		if outCloseErr != nil {
			return fmt.Errorf("close destination %s: %w", entry.path, outCloseErr)
		}
	}
	return nil
}

func entries(root string) ([]fileEntry, error) {
	if _, err := os.Stat(root); err != nil {
		return nil, err
	}
	rules, err := loadIgnoreRules(root)
	if err != nil {
		return nil, err
	}
	var out []fileEntry
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if fixedExcluded(rel) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if filepath.Base(rel) != ".gitignore" && ignored(rel, d.IsDir(), rules) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("source bundle does not support special file %s", rel)
		}
		out = append(out, fileEntry{path: rel, mode: info.Mode(), size: info.Size()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out, nil
}

func fixedExcluded(rel string) bool {
	for _, part := range strings.Split(rel, "/") {
		switch part {
		case ".git", ".airlock", "node_modules", ".cache", ".tmp":
			return true
		}
	}
	return false
}

func loadIgnoreRules(root string) ([]ignoreRule, error) {
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel != "." && fixedExcluded(filepath.ToSlash(rel)) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.IsDir() && d.Name() == ".gitignore" {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool {
		ri, _ := filepath.Rel(root, files[i])
		rj, _ := filepath.Rel(root, files[j])
		di, dj := strings.Count(filepath.ToSlash(ri), "/"), strings.Count(filepath.ToSlash(rj), "/")
		if di != dj {
			return di < dj
		}
		return ri < rj
	})
	var rules []ignoreRule
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", file, err)
		}
		base, err := filepath.Rel(root, filepath.Dir(file))
		if err != nil {
			return nil, err
		}
		if base == "." {
			base = ""
		} else {
			base = filepath.ToSlash(base)
		}
		for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
			rule, ok, err := compileIgnoreRule(base, line)
			if err != nil {
				return nil, fmt.Errorf("parse %s: %w", file, err)
			}
			if ok {
				rules = append(rules, rule)
			}
		}
	}
	return rules, nil
}

func compileIgnoreRule(base, line string) (ignoreRule, bool, error) {
	line = strings.TrimSuffix(line, "\r")
	if line == "" {
		return ignoreRule{}, false, nil
	}
	if strings.HasPrefix(line, `\#`) || strings.HasPrefix(line, `\!`) {
		line = line[1:]
	} else if strings.HasPrefix(line, "#") {
		return ignoreRule{}, false, nil
	}
	negated := strings.HasPrefix(line, "!")
	if negated {
		line = strings.TrimPrefix(line, "!")
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return ignoreRule{}, false, nil
	}
	dirOnly := strings.HasSuffix(line, "/")
	line = strings.TrimSuffix(line, "/")
	anchored := strings.HasPrefix(line, "/")
	line = strings.TrimPrefix(line, "/")
	hasSlash := strings.Contains(line, "/")
	body, err := globRegex(line)
	if err != nil {
		return ignoreRule{}, false, err
	}
	var expr string
	if anchored || hasSlash {
		expr = "^" + body
	} else {
		expr = `(?:^|/)` + body
	}
	if dirOnly || hasSlash || anchored {
		expr += `(?:$|/.*$)`
	} else {
		expr += `(?:$|/)`
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return ignoreRule{}, false, err
	}
	return ignoreRule{base: base, negated: negated, re: re}, true, nil
}

func globRegex(pattern string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(pattern); i++ {
		switch pattern[i] {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				i++
				if i+1 < len(pattern) && pattern[i+1] == '/' {
					i++
					b.WriteString(`(?:.*/)?`)
				} else {
					b.WriteString(`.*`)
				}
			} else {
				b.WriteString(`[^/]*`)
			}
		case '?':
			b.WriteString(`[^/]`)
		case '[':
			end := strings.IndexByte(pattern[i+1:], ']')
			if end < 0 {
				b.WriteString(`\[`)
				continue
			}
			end += i + 1
			class := pattern[i+1 : end]
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			b.WriteByte('[')
			b.WriteString(class)
			b.WriteByte(']')
			i = end
		case '\\':
			if i+1 < len(pattern) {
				i++
				b.WriteString(regexp.QuoteMeta(string(pattern[i])))
			} else {
				b.WriteString(`\\`)
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(pattern[i])))
		}
	}
	return b.String(), nil
}

func ignored(rel string, isDir bool, rules []ignoreRule) bool {
	ignored := false
	for _, rule := range rules {
		target := rel
		if rule.base != "" {
			if rel == rule.base {
				target = ""
			} else if strings.HasPrefix(rel, rule.base+"/") {
				target = strings.TrimPrefix(rel, rule.base+"/")
			} else {
				continue
			}
		}
		if target == "" && !isDir {
			continue
		}
		if rule.re.MatchString(target) {
			ignored = !rule.negated
		}
	}
	return ignored
}
