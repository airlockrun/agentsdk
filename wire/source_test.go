package wire

import "testing"

func TestSourceVersionETagRoundTrip(t *testing.T) {
	want := SourceVersion{Revision: "11111111-1111-1111-1111-111111111111", Generation: 7}
	got, err := ParseSourceETag(`"` + want.ETag() + `"`)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("ParseSourceETag() = %#v, want %#v", got, want)
	}
}

func TestParseSourceETagRejectsTreeState(t *testing.T) {
	if _, err := ParseSourceETag("sha256:abc"); err == nil {
		t.Fatal("ParseSourceETag accepted a tree-state hash")
	}
}
