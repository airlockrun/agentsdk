package sourcebundle

import (
	"archive/tar"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestDigestAndArchiveUseCanonicalFiles(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, ".gitignore", "*.log\nignored/\n!keep.log\n")
	writeTestFile(t, root, "main.go", "package main\n")
	writeTestFile(t, root, "debug.log", "ignored")
	writeTestFile(t, root, "keep.log", "kept")
	writeTestFile(t, root, "ignored/file.txt", "ignored")
	writeTestFile(t, root, ".git/config", "ignored")
	writeTestFile(t, root, ".airlock/local/agent.toml", "ignored")
	writeTestFile(t, root, "node_modules/pkg/index.js", "ignored")

	state1, err := Digest(root)
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	archivePath := filepath.Join(t.TempDir(), "source.tar.gz")
	f, err := os.Create(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	state2, err := WriteArchive(f, root)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatalf("WriteArchive: %v", err)
	}
	if state1 != state2 {
		t.Fatalf("states differ: %q != %q", state1, state2)
	}

	got := archiveNames(t, archivePath)
	want := []string{".gitignore", "keep.log", "main.go"}
	if len(got) != len(want) {
		t.Fatalf("archive names = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("archive names = %v, want %v", got, want)
		}
	}

	writeTestFile(t, root, "main.go", "package changed\n")
	state3, err := Digest(root)
	if err != nil {
		t.Fatal(err)
	}
	if state3 == state1 {
		t.Fatal("digest did not change with source content")
	}
	writeTestFile(t, root, "debug.log", "still ignored")
	state4, err := Digest(root)
	if err != nil {
		t.Fatal(err)
	}
	if state4 != state3 {
		t.Fatal("digest changed with ignored content")
	}
}

func TestNestedGitignore(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "sub/.gitignore", "*.tmp\n!keep.tmp\n")
	writeTestFile(t, root, "sub/drop.tmp", "drop")
	writeTestFile(t, root, "sub/keep.tmp", "keep")
	writeTestFile(t, root, "other/drop.tmp", "keep outside nested rules")

	entries, err := entries(root)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, entry := range entries {
		got = append(got, entry.path)
	}
	want := []string{"other/drop.tmp", "sub/.gitignore", "sub/keep.tmp"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entries = %v, want %v", got, want)
		}
	}
}

func TestMirrorPreservesLocalState(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	writeTestFile(t, src, "go.mod", "module new\n")
	writeTestFile(t, src, "new.txt", "new")
	writeTestFile(t, dst, "go.mod", "module old\n")
	writeTestFile(t, dst, "old.txt", "old")
	writeTestFile(t, dst, ".airlock/local/agent.toml", "state")
	writeTestFile(t, dst, ".git/config", "git")

	if err := Mirror(src, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dst, "old.txt")); !os.IsNotExist(err) {
		t.Fatalf("old source remains: %v", err)
	}
	for _, rel := range []string{"new.txt", ".airlock/local/agent.toml", ".git/config"} {
		if _, err := os.Stat(filepath.Join(dst, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("missing %s: %v", rel, err)
		}
	}
}

func writeTestFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func archiveNames(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var names []string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, hdr.Name)
	}
	sort.Strings(names)
	return names
}
