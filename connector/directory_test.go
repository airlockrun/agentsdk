package connector

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/airlockrun/agentsdk/connector/protocol"
)

func TestLocalDirectoryOperations(t *testing.T) {
	root := t.TempDir()
	directory := LocalDirectory(root)
	t.Cleanup(func() { _ = directory.Close() })
	entry, err := directory.Write("nested/file.txt", []byte("abcdef"), false)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Size != 6 {
		t.Fatalf("size = %d", entry.Size)
	}
	read, err := directory.Read("nested/file.txt", 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	if string(read.Data) != "cde" {
		t.Fatalf("read = %q", read.Data)
	}
	list, err := directory.List("nested", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Entries) != 1 || list.Entries[0].Path != "nested/file.txt" {
		t.Fatalf("list = %+v", list)
	}
	if _, err := directory.Write("nested/file.txt", []byte("x"), false); !errors.Is(err, os.ErrExist) {
		t.Fatalf("non-overwrite error = %v", err)
	}
	if err := directory.Move("nested/file.txt", "moved.txt", false); err != nil {
		t.Fatal(err)
	}
	if err := directory.Delete("moved.txt"); err != nil {
		t.Fatal(err)
	}
}

func TestLocalDirectoryListsEmptyDirectory(t *testing.T) {
	directory := LocalDirectory(t.TempDir())
	t.Cleanup(func() { _ = directory.Close() })
	result, err := directory.List("", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Entries) != 0 {
		t.Fatalf("entries = %+v", result.Entries)
	}
}

func TestDirectoryImportChecksumAndAtomicVisibility(t *testing.T) {
	body := []byte("verified transfer")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }))
	defer server.Close()
	directory := LocalDirectory(t.TempDir(), LocalDirectoryOptions{HTTPClient: server.Client()})
	t.Cleanup(func() { _ = directory.Close() })
	directory.setOrigins([]string{server.URL})
	sum := sha256.Sum256(body)
	entry, err := directory.Import(context.Background(), protocol.DirectoryImportRequest{Path: "good.bin", Grant: protocol.TransferGrant{URL: server.URL, ExpiresAt: time.Now().Add(time.Minute), MaximumSize: 1024, ExpectedSize: int64(len(body)), ExpectedSHA256: hex.EncodeToString(sum[:])}})
	if err != nil || entry.Size != int64(len(body)) {
		t.Fatalf("entry = %+v, error = %v", entry, err)
	}
	_, err = directory.Import(context.Background(), protocol.DirectoryImportRequest{Path: "bad.bin", Grant: protocol.TransferGrant{URL: server.URL, ExpiresAt: time.Now().Add(time.Minute), MaximumSize: 1024, ExpectedSHA256: strings.Repeat("0", 64)}})
	if err == nil {
		t.Fatal("checksum mismatch succeeded")
	}
	if _, statErr := directory.Stat("bad.bin"); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("partial destination exists: %v", statErr)
	}
}

func TestDirectoryTransferLimitsAndRedirects(t *testing.T) {
	var destinationCalls int
	destination := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { destinationCalls++ }))
	defer destination.Close()
	source := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	directory := LocalDirectory(t.TempDir(), LocalDirectoryOptions{HTTPClient: source.Client()})
	t.Cleanup(func() { _ = directory.Close() })
	directory.setOrigins([]string{source.URL, destination.URL})
	_, err := directory.Import(context.Background(), protocol.DirectoryImportRequest{Path: "redirect.bin", Grant: protocol.TransferGrant{URL: source.URL, ExpiresAt: time.Now().Add(time.Minute), MaximumSize: 1024}})
	if err == nil || destinationCalls != 0 {
		t.Fatalf("redirect error = %v, destination calls = %d", err, destinationCalls)
	}
	if _, err := directory.Write("export.bin", []byte("data"), false); err != nil {
		t.Fatal(err)
	}
	_, err = directory.Export(context.Background(), protocol.DirectoryExportRequest{Path: "export.bin", PartSize: protocol.MaxTransferPartBytes + 1, Parts: []protocol.UploadPartGrant{{Number: 1, URL: source.URL}}, Grant: protocol.TransferGrant{URL: source.URL, ExpiresAt: time.Now().Add(time.Minute), MaximumSize: 1024}})
	if err == nil {
		t.Fatal("oversized transfer part succeeded")
	}
}

func TestDirectoryTransferRedirectsAreHopLimited(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, server.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	directory := LocalDirectory(t.TempDir(), LocalDirectoryOptions{HTTPClient: server.Client()})
	t.Cleanup(func() { _ = directory.Close() })
	directory.setOrigins([]string{server.URL})
	_, err := directory.Import(context.Background(), protocol.DirectoryImportRequest{Path: "redirect.bin", Grant: protocol.TransferGrant{URL: server.URL, ExpiresAt: time.Now().Add(time.Minute), MaximumSize: 1024}})
	if err == nil || !strings.Contains(err.Error(), "10 transfer redirects") {
		t.Fatalf("redirect error = %v", err)
	}
}

func TestLocalDirectoryRejectsEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	directory := LocalDirectory(root)
	t.Cleanup(func() { _ = directory.Close() })
	tests := []string{"../secret", "/etc/passwd", "a/../secret", `escape\secret`, "escape/secret"}
	for _, value := range tests {
		t.Run(value, func(t *testing.T) {
			if _, err := directory.Read(value, 0, 0); err == nil {
				t.Fatalf("Read(%q) succeeded", value)
			}
		})
	}
}
