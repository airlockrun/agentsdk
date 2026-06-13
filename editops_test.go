package agentsdk

import (
	"context"
	"strings"
	"testing"
)

// editTo runs a line edit and returns the resulting dst content from the mock.
func editTo(t *testing.T, r *run, mock *storageMock, src, dst string, edits []lineEdit) string {
	t.Helper()
	if _, err := r.editLines(context.Background(), src, dst, edits); err != nil {
		t.Fatalf("editLines: %v", err)
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	return string(mock.files[dst])
}

// sedTo runs a sed script and returns the resulting dst content.
func sedTo(t *testing.T, r *run, mock *storageMock, src, dst, script string) string {
	t.Helper()
	if _, err := r.sed(context.Background(), src, script, dst); err != nil {
		t.Fatalf("sed: %v", err)
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	return string(mock.files[dst])
}

const fiveLines = "l1\nl2\nl3\nl4\nl5\n"

func TestEditLines(t *testing.T) {
	tests := []struct {
		name  string
		edits []lineEdit
		want  string
	}{
		{"replace range", []lineEdit{{from: 2, count: 2, text: "X\n", hasText: true}}, "l1\nX\nl4\nl5\n"},
		{"delete range", []lineEdit{{from: 2, count: 2}}, "l1\nl4\nl5\n"},
		{"insert before", []lineEdit{{from: 3, count: 0, text: "ins\n", hasText: true}}, "l1\nl2\nins\nl3\nl4\nl5\n"},
		{"append", []lineEdit{{isAppend: true, text: "end\n", hasText: true}}, fiveLines + "end\n"},
		{"multi-edit one pass", []lineEdit{
			{from: 1, count: 1, text: "TOP\n", hasText: true},
			{from: 4, count: 0, text: "mid\n", hasText: true},
			{isAppend: true, text: "END\n", hasText: true},
		}, "TOP\nl2\nl3\nmid\nl4\nl5\nEND\n"},
		{"multibyte text", []lineEdit{
			{from: 2, count: 1, text: "日本語🎉\n", hasText: true},
			{isAppend: true, text: "café ☕\n", hasText: true},
		}, "l1\n日本語🎉\nl3\nl4\nl5\ncafé ☕\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, mock, r := storageAgent(t)
			mock.put("f", []byte(fiveLines))
			if got := editTo(t, r, mock, "f", "out", tt.edits); got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestEditLinesOverlapRejected(t *testing.T) {
	_, mock, r := storageAgent(t)
	mock.put("f", []byte(fiveLines))
	_, err := r.editLines(context.Background(), "f", "out", []lineEdit{
		{from: 2, count: 2, text: "a\n", hasText: true},
		{from: 3, count: 1, text: "b\n", hasText: true}, // overlaps 2-3
	})
	if err == nil || !strings.Contains(err.Error(), "overlap") {
		t.Fatalf("expected overlap rejection, got %v", err)
	}
}

func TestSed(t *testing.T) {
	tests := []struct {
		name, script, in, want string
	}{
		{"subst global", "s/o/0/g", "foo boo\n", "f00 b00\n"},
		{"subst first only", "s/o/0/", "foo\n", "f0o\n"},
		{"subst ignorecase", "s/ab/X/gi", "ab AB Ab\n", "X X X\n"},
		{"subst backref ($1)", "s/(a)(b)/$2$1/", "ab\n", "ba\n"},
		{"delete by regex", "/^#/d", "#c\nkeep\n#d\n", "keep\n"},
		{"delete range", "2,3d", fiveLines, "l1\nl4\nl5\n"},
		{"change line", "2c\\CHANGED", fiveLines, "l1\nCHANGED\nl3\nl4\nl5\n"},
		{"insert before line", "2i\\INS", fiveLines, "l1\nINS\nl2\nl3\nl4\nl5\n"},
		{"append after line", "2a\\APP", fiveLines, "l1\nl2\nAPP\nl3\nl4\nl5\n"},
		{"last-line append", "$a\\TAIL", "x\ny\n", "x\ny\nTAIL\n"},
		{"multi-command", "s/l/L/g\n/L3/d", fiveLines, "L1\nL2\nL4\nL5\n"},

		// Address × command combinations.
		{"line-addressed subst", "2s/l/L/", fiveLines, "l1\nL2\nl3\nl4\nl5\n"},
		{"range-addressed subst", "2,4s/l/L/g", fiveLines, "l1\nL2\nL3\nL4\nl5\n"},
		{"regex-addressed subst", "/l3/s/l/L/", fiveLines, "l1\nl2\nL3\nl4\nl5\n"},
		{"range change emits once", "2,4c\\X", fiveLines, "l1\nX\nl5\n"},
		{"last-line subst", "$s/l/L/", fiveLines, "l1\nl2\nl3\nl4\nL5\n"},

		// Parser features.
		{"custom delimiter", "s#l1#X#", fiveLines, "X\nl2\nl3\nl4\nl5\n"},
		{"escaped delimiter", `s/a\/b/X/`, "a/b\n", "X\n"},
		{"comment and blank lines", "# note\n\ns/a/b/", "a\n", "b\n"},
		{"two substs on one line", "s/a/b/\ns/b/c/", "a\n", "c\n"},
		{"no trailing newline in", "s/x/Y/", "x", "Y\n"},

		// Multibyte / CJK / emoji — Go regexp is rune-aware and subst slices on
		// byte indices, so these exercise the rune↔byte boundaries.
		{"subst into CJK", "s/cat/猫/g", "cat cat\n", "猫 猫\n"},
		{"subst into emoji", "s/world/🌍/", "world\n", "🌍\n"},
		{"subst after multibyte (byte offsets)", "s/x/Y/", "日本x\n", "日本Y\n"},
		{"global subst around multibyte", "s/-/=/g", "a-本-b\n", "a=本=b\n"},
		{"capture multibyte ($1)", "s/(.+)/<$1>/", "世界\n", "<世界>\n"},
		{"delete CJK-matching line", "/日本/d", "ok\n日本語\nbye\n", "ok\nbye\n"},
		{"append emoji at EOF", "$a\\🎉", "x\n", "x\n🎉\n"},
		{"change to accented", "1c\\café", "x\ny\n", "café\ny\n"},
		{"multibyte pattern and repl", "s/Ω/Ω²/", "Ω\n", "Ω²\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, mock, r := storageAgent(t)
			mock.put("f", []byte(tt.in))
			if got := sedTo(t, r, mock, "f", "out", tt.script); got != tt.want {
				t.Fatalf("script %q: got %q, want %q", tt.script, got, tt.want)
			}
		})
	}
}

func TestSedParseErrors(t *testing.T) {
	_, mock, r := storageAgent(t)
	mock.put("f", []byte("a\nb\n"))
	cases := []struct{ name, script, wantSubstr string }{
		{"unknown command", "2x", "unknown command"},
		{"missing trailing delimiter", "s/a/b", "missing closing"},
		{"unknown s flag", "s/a/b/z", "unknown s flag"},
		{"bad subst regex", "s/(/b/", "s regex"},
		{"bad address regex", "/(/d", "address regex"},
		{"bad range address", "2,x d", "bad range address"},
		{"empty script", "  \n# only a comment\n", "empty sed script"},
		{"empty s command", "s", "empty s command"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := r.sed(context.Background(), "f", c.script, "out")
			if err == nil {
				t.Fatalf("expected an error for %q", c.script)
			}
			if !strings.Contains(err.Error(), c.wantSubstr) {
				t.Fatalf("error %q does not contain %q", err.Error(), c.wantSubstr)
			}
		})
	}
}

func TestEditInPlace(t *testing.T) {
	_, mock, r := storageAgent(t)
	mock.put("doc.txt", []byte("hello world\n"))
	// dst === src → overwrite the source.
	res, err := r.sed(context.Background(), "doc.txt", "s/world/there/", "doc.txt")
	if err != nil {
		t.Fatalf("in-place sed: %v", err)
	}
	if res.savedTo != "doc.txt" {
		t.Fatalf("expected savedTo=doc.txt, got %q", res.savedTo)
	}
	mock.mu.Lock()
	got := string(mock.files["doc.txt"])
	mock.mu.Unlock()
	if got != "hello there\n" {
		t.Fatalf("in-place result = %q", got)
	}
}

func TestEditBigFileStreams(t *testing.T) {
	withSizeForCache(t, 1024) // force the source to spill to the disk cache
	_, mock, r := storageAgent(t)
	var sb strings.Builder
	for i := 0; i < 2000; i++ {
		sb.WriteString("line\n")
	}
	big := sb.String()
	mock.put("big.log", []byte(big))

	got := sedTo(t, r, mock, "big.log", "out", "s/line/LINE/")
	if got != strings.ReplaceAll(big, "line", "LINE") {
		t.Fatalf("big-file sed mismatch (len got %d want %d)", len(got), len(big))
	}
	if mock.gets("big.log") != 1 {
		t.Fatalf("big source should be fetched once (cached); got %d GETs", mock.gets("big.log"))
	}
}
