package agentsdk

import (
	"fmt"
	"testing"
)

// TestJSFileOps exercises the new file bindings through the real goja VM so the
// argument parsing, access checks, and return shapes are covered end to end.
func TestJSFileOps(t *testing.T) {
	_, _, r := storageAgent(t)
	vm := r.vmRuntime()

	if _, err := executeJS(vm, `fileWrite("tmp/data.txt", "alpha\nbeta\ngamma\n")`); err != nil {
		t.Fatalf("writeFile: %v", err)
	}

	cases := []struct{ name, code, want string }{
		{"readFile", `fileRead("tmp/data.txt")`, "alpha\nbeta\ngamma\n"},
		{"grep", `fileGrep("tmp/data.txt", "gamma")`, "gamma\n"},
		{"head", `fileHead("tmp/data.txt", 1)`, "alpha\n"},
		{"tail", `fileTail("tmp/data.txt", 1)`, "gamma\n"},
		{"readLines", `fileLines("tmp/data.txt", 2, 1)`, "beta\n"},
		{"encodeFile inline", `fileEncode("tmp/data.txt", "base64").content`, "YWxwaGEKYmV0YQpnYW1tYQo="},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := executeJS(vm, c.code)
			if err != nil {
				t.Fatalf("%s: %v", c.code, err)
			}
			if got != c.want {
				t.Fatalf("%s = %q, want %q", c.code, got, c.want)
			}
		})
	}
}

// TestJSEditors drives fileEditLines (object + array forms) and fileSed
// (including in-place) through the real VM.
func TestJSEditors(t *testing.T) {
	_, mock, r := storageAgent(t)
	mock.put("tmp/e.txt", []byte("a\nb\nc\n"))
	vm := r.vmRuntime()

	// fileEditLines, single object, explicit dst.
	if _, err := executeJS(vm, `fileEditLines("tmp/e.txt", {from:2, count:1, text:"B\n"}, "tmp/e2.txt")`); err != nil {
		t.Fatalf("fileEditLines object: %v", err)
	}
	if got := mockFile(mock, "tmp/e2.txt"); got != "a\nB\nc\n" {
		t.Fatalf("fileEditLines object → %q", got)
	}

	// fileEditLines, array of edits.
	if _, err := executeJS(vm, `fileEditLines("tmp/e.txt", [{from:1,count:0,text:"top\n"},{append:"end\n"}], "tmp/e3.txt")`); err != nil {
		t.Fatalf("fileEditLines array: %v", err)
	}
	if got := mockFile(mock, "tmp/e3.txt"); got != "top\na\nb\nc\nend\n" {
		t.Fatalf("fileEditLines array → %q", got)
	}

	// fileSed in place (dst === src).
	if _, err := executeJS(vm, `fileSed("tmp/e.txt", "s/a/A/", "tmp/e.txt")`); err != nil {
		t.Fatalf("fileSed in-place: %v", err)
	}
	if got := mockFile(mock, "tmp/e.txt"); got != "A\nb\nc\n" {
		t.Fatalf("fileSed in-place → %q", got)
	}

	// fileSed to scratch, small result returns inline content.
	out, err := executeJS(vm, `fileSed("tmp/e.txt", "/b/d").content`)
	if err != nil {
		t.Fatalf("fileSed scratch: %v", err)
	}
	if out != "A\nc\n" {
		t.Fatalf("fileSed scratch inline → %q", out)
	}
}

func mockFile(m *storageMock, key string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return string(m.files[key])
}

// TestJSMultibyteRoundTrip pushes CJK/accented/emoji text through the Go↔goja
// boundary (UTF-8 ↔ UTF-16; emoji are surrogate pairs in JS) and checks the
// exact bytes of a 4-byte rune via fileReadRangeBytes.
func TestJSMultibyteRoundTrip(t *testing.T) {
	_, _, r := storageAgent(t)
	vm := r.vmRuntime()

	if _, err := executeJS(vm, `fileWrite("tmp/m.txt", "héllo 世界 🎉\n")`); err != nil {
		t.Fatalf("fileWrite: %v", err)
	}
	got, err := executeJS(vm, `fileRead("tmp/m.txt")`)
	if err != nil {
		t.Fatalf("fileRead: %v", err)
	}
	if got != "héllo 世界 🎉\n" {
		t.Fatalf("round-trip mismatch: %q", got)
	}

	// 🎉 (U+1F389 = F0 9F 8E 89) starts right after "héllo 世界 ".
	off := len("héllo 世界 ")
	code := fmt.Sprintf(`(function(){var u=fileReadRangeBytes("tmp/m.txt",%d,4);return [u[0],u[1],u[2],u[3]].join(",");})()`, off)
	got, err = executeJS(vm, code)
	if err != nil {
		t.Fatalf("fileReadRangeBytes: %v", err)
	}
	if got != "240,159,142,137" { // F0 9F 8E 89
		t.Fatalf("emoji bytes = %q, want 240,159,142,137", got)
	}

	// A sed substitution touching multibyte content round-trips too.
	out, err := executeJS(vm, `fileSed("tmp/m.txt", "s/世界/🌏/").content`)
	if err != nil {
		t.Fatalf("fileSed: %v", err)
	}
	if out != "héllo 🌏 🎉\n" {
		t.Fatalf("sed multibyte = %q", out)
	}
}

func TestJSReadRangeBytesIsTypedArray(t *testing.T) {
	_, _, r := storageAgent(t)
	vm := r.vmRuntime()
	if _, err := executeJS(vm, `fileWrite("tmp/b.bin", "ABCDEF")`); err != nil {
		t.Fatalf("writeFile: %v", err)
	}
	// fileReadRangeBytes(...) must behave like a Uint8Array.
	got, err := executeJS(vm, `(function(){ var u = fileReadRangeBytes("tmp/b.bin", 1, 3); return u.length + ":" + u[0]; })()`)
	if err != nil {
		t.Fatalf("readRangeBytes: %v", err)
	}
	if got != "3:66" { // bytes "BCD"; 'B' == 66
		t.Fatalf("got %q, want %q", got, "3:66")
	}
}
