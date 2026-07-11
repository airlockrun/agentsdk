package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/airlockrun/agentsdk"
	"github.com/airlockrun/agentsdk/scaffold"
)

const manifestName = "manifest.json"

type manifest struct {
	TemplVersion string            `json:"templ_version"`
	DaisyVersion string            `json:"daisyui_version"`
	HTMXVersion  string            `json:"htmx_version"`
	Sources      map[string]string `json:"sources"`
	Files        map[string]string `json:"files"`
}

var client = &http.Client{Timeout: 5 * time.Minute}

func main() {
	check := flag.Bool("check", false, "verify the checked-in skill bundle without network access")
	flag.Parse()
	if flag.NArg() != 0 {
		fatal(errors.New("no positional arguments accepted"))
	}

	root, err := repoRoot()
	if err != nil {
		fatal(err)
	}
	dir := filepath.Join(root, "scaffold", "skills")
	if *check {
		err = checkBundle(dir)
	} else {
		err = syncBundle(dir)
	}
	if err != nil {
		fatal(err)
	}
	if *check {
		fmt.Println("agentsdk scaffold skills: OK")
	} else {
		fmt.Printf("Updated agentsdk scaffold skills in %s\n", dir)
	}
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "syncskills: %v\n", err)
	os.Exit(1)
}

func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil && strings.Contains(string(data), "module github.com/airlockrun/agentsdk") {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("agentsdk repository root not found")
		}
		dir = parent
	}
}

func syncBundle(dst string) error {
	parent := filepath.Dir(dst)
	tmp, err := os.MkdirTemp(parent, ".skills-sync-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	for _, skill := range []string{"templ", "htmx"} {
		src := filepath.Join(dst, skill, "SKILL.md")
		data, err := os.ReadFile(src)
		if err != nil {
			return fmt.Errorf("read authored %s router: %w", skill, err)
		}
		if err := writeFile(filepath.Join(tmp, skill, "SKILL.md"), data); err != nil {
			return err
		}
	}

	daisyArchive := fmt.Sprintf("https://github.com/saadeghi/daisyui/archive/refs/tags/%s.tar.gz", scaffold.DaisyUIVersion)
	if err := extractTarGz(daisyArchive, tmp, "/skills/daisyui/", "daisyui", func(rel string, data []byte) (string, []byte, bool, error) {
		if rel == "install" || strings.HasPrefix(rel, "install/") {
			return "", nil, false, nil
		}
		if rel == "SKILL.md" {
			if !strings.Contains(string(data), "install/SKILL.md") {
				return "", nil, false, errors.New("daisyui SKILL.md no longer references install/SKILL.md")
			}
			var lines []string
			for _, line := range strings.Split(string(data), "\n") {
				if !strings.Contains(line, "install/SKILL.md") {
					lines = append(lines, line)
				}
			}
			data = []byte(strings.Join(lines, "\n"))
		}
		return rel, data, true, nil
	}); err != nil {
		return fmt.Errorf("sync daisyui skill: %w", err)
	}

	templArchive := fmt.Sprintf("https://github.com/a-h/templ/archive/refs/tags/%s.tar.gz", scaffold.TemplVersion)
	for _, docsDir := range []string{"03-syntax-and-usage", "04-core-concepts"} {
		suffix := "/docs/docs/" + docsDir + "/"
		if err := extractTarGz(templArchive, tmp, suffix, filepath.Join("templ", "reference", docsDir), keepFile); err != nil {
			return fmt.Errorf("sync templ %s: %w", docsDir, err)
		}
	}

	htmxTag := "v" + strings.TrimPrefix(agentsdk.HTMXVersion, "v")
	for _, name := range []string{"docs.md", "reference.md"} {
		url := fmt.Sprintf("https://raw.githubusercontent.com/bigskysoftware/htmx/%s/www/content/%s", htmxTag, name)
		data, err := download(url)
		if err != nil {
			return fmt.Errorf("sync htmx %s: %w", name, err)
		}
		if err := writeFile(filepath.Join(tmp, "htmx", "reference", name), data); err != nil {
			return err
		}
	}

	licenses := []struct {
		skill string
		url   string
	}{
		{"daisyui", fmt.Sprintf("https://raw.githubusercontent.com/saadeghi/daisyui/%s/LICENSE", scaffold.DaisyUIVersion)},
		{"templ", fmt.Sprintf("https://raw.githubusercontent.com/a-h/templ/%s/LICENSE", scaffold.TemplVersion)},
		{"htmx", fmt.Sprintf("https://raw.githubusercontent.com/bigskysoftware/htmx/%s/LICENSE", htmxTag)},
	}
	for _, license := range licenses {
		data, err := download(license.url)
		if err != nil {
			return fmt.Errorf("sync %s license: %w", license.skill, err)
		}
		if err := writeFile(filepath.Join(tmp, license.skill, "UPSTREAM_LICENSE"), data); err != nil {
			return err
		}
	}

	if err := validateRequiredFiles(tmp); err != nil {
		return err
	}
	m := manifest{
		TemplVersion: scaffold.TemplVersion,
		DaisyVersion: scaffold.DaisyUIVersion,
		HTMXVersion:  agentsdk.HTMXVersion,
		Sources: map[string]string{
			"daisyui": daisyArchive + " (skills/daisyui, install subskill removed)",
			"templ":   templArchive + " (docs/docs/03-syntax-and-usage and 04-core-concepts)",
			"htmx":    fmt.Sprintf("https://github.com/bigskysoftware/htmx/tree/%s/www/content", htmxTag),
		},
	}
	m.Files, err = fileHashes(tmp)
	if err != nil {
		return err
	}
	manifestData, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	manifestData = append(manifestData, '\n')
	if err := writeFile(filepath.Join(tmp, manifestName), manifestData); err != nil {
		return err
	}

	if err := checkBundle(tmp); err != nil {
		return fmt.Errorf("generated bundle is invalid: %w", err)
	}
	backup := dst + ".old"
	if err := os.RemoveAll(backup); err != nil {
		return err
	}
	if err := os.Rename(dst, backup); err != nil {
		return fmt.Errorf("move existing skill bundle: %w", err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Rename(backup, dst)
		return fmt.Errorf("install skill bundle: %w", err)
	}
	if err := os.RemoveAll(backup); err != nil {
		return err
	}
	return nil
}

func checkBundle(dir string) error {
	data, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil {
		return fmt.Errorf("read manifest: %w; run go run ./internal/cmd/syncskills", err)
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return fmt.Errorf("parse manifest: %w", err)
	}
	if m.TemplVersion != scaffold.TemplVersion || m.DaisyVersion != scaffold.DaisyUIVersion || m.HTMXVersion != agentsdk.HTMXVersion {
		return fmt.Errorf("manifest versions are stale; run go run ./internal/cmd/syncskills")
	}
	if err := validateRequiredFiles(dir); err != nil {
		return err
	}
	got, err := fileHashes(dir)
	if err != nil {
		return err
	}
	if len(got) != len(m.Files) {
		return fmt.Errorf("skill file set differs from manifest; run go run ./internal/cmd/syncskills")
	}
	for path, want := range m.Files {
		if got[path] != want {
			return fmt.Errorf("skill file %s differs from manifest; run go run ./internal/cmd/syncskills", path)
		}
	}
	return nil
}

func validateRequiredFiles(dir string) error {
	required := []string{
		"daisyui/SKILL.md",
		"templ/SKILL.md",
		"templ/reference/03-syntax-and-usage/06-if-else.md",
		"templ/reference/04-core-concepts/01-components.md",
		"htmx/SKILL.md",
		"htmx/reference/docs.md",
		"htmx/reference/reference.md",
	}
	for _, path := range required {
		if info, err := os.Stat(filepath.Join(dir, filepath.FromSlash(path))); err != nil || info.IsDir() {
			return fmt.Errorf("required skill file %s is missing", path)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "daisyui", "install")); !os.IsNotExist(err) {
		return errors.New("daisyui install subskill must be absent")
	}
	daisy, err := os.ReadFile(filepath.Join(dir, "daisyui", "SKILL.md"))
	if err != nil {
		return err
	}
	if strings.Contains(string(daisy), "install/SKILL.md") {
		return errors.New("daisyui SKILL.md still references the removed install subskill")
	}
	versions := map[string]string{
		"templ": "version: " + scaffold.TemplVersion,
		"htmx":  "version: v" + strings.TrimPrefix(agentsdk.HTMXVersion, "v"),
	}
	daisyParts := strings.Split(strings.TrimPrefix(scaffold.DaisyUIVersion, "v"), ".")
	if len(daisyParts) < 2 {
		return fmt.Errorf("invalid daisyui version %q", scaffold.DaisyUIVersion)
	}
	versions["daisyui"] = "version: " + daisyParts[0] + "." + daisyParts[1] + ".x"
	for skill, version := range versions {
		data, err := os.ReadFile(filepath.Join(dir, skill, "SKILL.md"))
		if err != nil {
			return err
		}
		if !strings.Contains(string(data), "name: "+skill) || !strings.Contains(string(data), version) {
			return fmt.Errorf("%s skill metadata does not match %s", skill, version)
		}
	}
	return nil
}

func fileHashes(dir string) (map[string]string, error) {
	hashes := make(map[string]string)
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || entry.Name() == manifestName {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		hashes[filepath.ToSlash(rel)] = hex.EncodeToString(sum[:])
		return nil
	})
	return hashes, err
}

type transform func(rel string, data []byte) (string, []byte, bool, error)

func keepFile(rel string, data []byte) (string, []byte, bool, error) {
	return rel, data, true, nil
}

func extractTarGz(url, dst, suffix, outputPrefix string, transform transform) error {
	data, err := download(url)
	if err != nil {
		return err
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	var found int
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		idx := strings.Index(header.Name, suffix)
		if idx < 0 || header.Typeflag != tar.TypeReg {
			continue
		}
		rel := strings.TrimPrefix(header.Name[idx+len(suffix):], "/")
		if rel == "" || !safeRelativePath(rel) {
			continue
		}
		contents, err := io.ReadAll(tr)
		if err != nil {
			return err
		}
		rel, contents, keep, err := transform(rel, contents)
		if err != nil {
			return err
		}
		if !keep {
			continue
		}
		if !safeRelativePath(rel) {
			return fmt.Errorf("unsafe archive path %q", rel)
		}
		if err := writeFile(filepath.Join(dst, outputPrefix, filepath.FromSlash(rel)), contents); err != nil {
			return err
		}
		found++
	}
	if found == 0 {
		return fmt.Errorf("no files found under archive suffix %s", suffix)
	}
	return nil
}

func safeRelativePath(path string) bool {
	clean := filepath.Clean(filepath.FromSlash(path))
	return clean != "." && !filepath.IsAbs(clean) && clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

func download(url string) ([]byte, error) {
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

func writeFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
