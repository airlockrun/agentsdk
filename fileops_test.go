package agentsdk

import (
	"context"
	"strings"
	"testing"
)

func TestReadRangeUncachedUsesS3Range(t *testing.T) {
	withSizeForCache(t, 1<<20) // high, so the small file isn't cached
	_, mock, r := storageAgent(t)
	mock.put("data/f.txt", []byte("0123456789"))

	got, err := r.readRange(context.Background(), "data/f.txt", 2, 3)
	if err != nil {
		t.Fatalf("readRange: %v", err)
	}
	if string(got) != "234" {
		t.Fatalf("got %q, want %q", got, "234")
	}
	if len(r.fileCache.entries) != 0 {
		t.Fatal("readRange must not populate the cache")
	}
}

func TestReadRangeCachedSeeksLocally(t *testing.T) {
	withSizeForCache(t, 8)
	_, mock, r := storageAgent(t)
	mock.put("data/g.txt", []byte("abcdefghijklmnop")) // 16 bytes > threshold

	readAllClose(mustOpen(t, r, "data/g.txt")) // spill → cache (GET #1)
	gotsBefore := mock.gets("data/g.txt")

	got, err := r.readRange(context.Background(), "data/g.txt", 4, 5)
	if err != nil {
		t.Fatalf("readRange: %v", err)
	}
	if string(got) != "efghi" {
		t.Fatalf("got %q, want %q", got, "efghi")
	}
	if mock.gets("data/g.txt") != gotsBefore {
		t.Fatal("a cached readRange must seek locally, not issue another GET")
	}
}

const grepText = "alpha\nBeta\ngamma\nbeta two\ndelta\n"

func TestGrep(t *testing.T) {
	_, mock, r := storageAgent(t)
	mock.put("log/app.log", []byte(grepText))

	got, err := r.grepFile(context.Background(), "log/app.log", "beta", grepOpts{})
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	if got != "beta two\n" {
		t.Fatalf("case-sensitive grep got %q", got)
	}

	got, _ = r.grepFile(context.Background(), "log/app.log", "beta", grepOpts{ignoreCase: true})
	if got != "Beta\nbeta two\n" {
		t.Fatalf("ignoreCase grep got %q", got)
	}

	got, _ = r.grepFile(context.Background(), "log/app.log", "beta", grepOpts{ignoreCase: true, lineNumbers: true})
	if got != "2:Beta\n4:beta two\n" {
		t.Fatalf("lineNumbers grep got %q", got)
	}

	got, _ = r.grepFile(context.Background(), "log/app.log", "beta", grepOpts{invert: true})
	if got != "alpha\nBeta\ngamma\ndelta\n" {
		t.Fatalf("invert grep got %q", got)
	}
}

func TestGrepTruncates(t *testing.T) {
	_, mock, r := storageAgent(t)
	mock.put("log/many.log", []byte(strings.Repeat("match\n", 10)))

	got, err := r.grepFile(context.Background(), "log/many.log", "match", grepOpts{max: 3})
	if err != nil {
		t.Fatalf("grep: %v", err)
	}
	if strings.Count(got, "match\n") != 3 {
		t.Fatalf("expected 3 emitted matches, got %q", got)
	}
	if !strings.Contains(got, "7 more match") {
		t.Fatalf("expected a truncation note, got %q", got)
	}
}

func TestFileOpsMultibyte(t *testing.T) {
	_, mock, r := storageAgent(t)
	// Lines splitting on '\n' (0x0A) can never cut a UTF-8 rune (continuation
	// bytes are >= 0x80), so the line ops must be byte-transparent.
	mock.put("m", []byte("café\n世界\n🎉 party\nдобро\n"))

	if got, _ := r.grepFile(context.Background(), "m", "世", grepOpts{}); got != "世界\n" {
		t.Fatalf("grep CJK got %q", got)
	}
	if got, _ := r.headLines(context.Background(), "m", 1); got != "café\n" {
		t.Fatalf("head got %q", got)
	}
	if got, _ := r.tailLines(context.Background(), "m", 1); got != "добро\n" {
		t.Fatalf("tail got %q", got)
	}
	if got, _ := r.readLineWindow(context.Background(), "m", 2, 2); got != "世界\n🎉 party\n" {
		t.Fatalf("readLines got %q", got)
	}
	// Exact byte window over the emoji (🎉 = F0 9F 8E 89, the line-3 prefix).
	emojiStart := int64(len("café\n世界\n")) // byte offset where line 3 begins
	got, err := r.readRange(context.Background(), "m", emojiStart, 4)
	if err != nil {
		t.Fatalf("readRange: %v", err)
	}
	if string(got) != "🎉" {
		t.Fatalf("byte window over emoji = % x, want F0 9F 8E 89", got)
	}
}

func TestHeadTailReadLines(t *testing.T) {
	_, mock, r := storageAgent(t)
	mock.put("f", []byte("l1\nl2\nl3\nl4\nl5\n"))

	if got, _ := r.headLines(context.Background(), "f", 2); got != "l1\nl2\n" {
		t.Fatalf("head got %q", got)
	}
	if got, _ := r.tailLines(context.Background(), "f", 2); got != "l4\nl5\n" {
		t.Fatalf("tail got %q", got)
	}
	if got, _ := r.readLineWindow(context.Background(), "f", 2, 2); got != "l2\nl3\n" {
		t.Fatalf("readLines got %q", got)
	}
}
