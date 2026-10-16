package csvexample

import (
	"context"
	"encoding/csv"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestCSVPreservesCells(t *testing.T) {
	in := Input{Header: []string{"name", "note", "quantity"}, Rows: [][]string{{"Alice", `comma, quote "ok"`, "2"}, {"Bob", "two\nlines", "3"}, {"Саша", "Unicode", "4"}}}
	dir := t.TempDir()
	out, err := WriteCSV(t.Context(), dir, in)
	if err != nil || out.Rows != len(in.Rows) || out.Filename != Filename {
		t.Fatalf("output=%+v error=%v", out, err)
	}
	file, err := os.Open(filepath.Join(dir, Filename))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	rows, err := csv.NewReader(file).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rows, append([][]string{in.Header}, in.Rows...)) {
		t.Fatalf("records=%q", rows)
	}
}
func TestCSVInvalidOrCancelledDoesNotWrite(t *testing.T) {
	for _, in := range []Input{{}, {Header: []string{"a"}, Rows: [][]string{{"x", "y"}}}} {
		dir := t.TempDir()
		if _, err := WriteCSV(t.Context(), dir, in); err == nil {
			t.Fatal("accepted invalid rows")
		}
		if _, err := os.Stat(filepath.Join(dir, Filename)); !os.IsNotExist(err) {
			t.Fatal("invalid rows wrote an artifact")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := WriteCSV(ctx, t.TempDir(), Input{Header: []string{"a"}}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
