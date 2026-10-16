package sourcebundle

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

type archiveEditWriter struct {
	bytes.Buffer
	edit func()
}

func (w *archiveEditWriter) Write(body []byte) (int, error) {
	if w.edit != nil {
		edit := w.edit
		w.edit = nil
		edit()
	}
	return w.Buffer.Write(body)
}

func TestArchiveStateDescribesActualStreamDuringSourceEdit(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "main.go")
	if err := os.WriteFile(path, []byte("package old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := Digest(root)
	if err != nil {
		t.Fatal(err)
	}
	writer := &archiveEditWriter{edit: func() {
		if err := os.WriteFile(path, []byte("package new\n"), 0o644); err != nil {
			t.Error(err)
		}
	}}
	state, err := WriteArchive(writer, root)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := ExtractArchiveState(bytes.NewReader(writer.Bytes()), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if actual != state || actual == before {
		t.Fatalf("archive actual=%s returned=%s pre-edit=%s", actual, state, before)
	}
}
