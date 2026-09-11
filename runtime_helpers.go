package agentsdk

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/airlockrun/agentsdk/capability"
	"github.com/airlockrun/goai/tool"
)

// File reads across the capability boundary are bounded; larger files use
// streaming line operations or byte ranges.
const maxReadFileBytes = 16 << 20

// Attachment data uses a storage reference resolved by the model host.
const s3RefSentinel = "s3ref:"

func readCappedFile(rc io.ReadCloser) ([]byte, error) {
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, maxReadFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxReadFileBytes {
		return nil, errors.New("file exceeds the 16 MiB in-memory cap; use fileGrep, fileHead, fileTail, fileLines or fileReadRangeBytes")
	}
	return b, nil
}

func rowsToMaps(rows *sql.Rows) ([]map[string]any, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var result []map[string]any
	for rows.Next() {
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := make(map[string]any, len(cols))
		for i, col := range cols {
			v := values[i]
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			row[col] = v
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return fmt.Sprintf("%x", b)
}

func jsonResult(v any) (tool.Result, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return tool.Result{}, fmt.Errorf("encode tool output: %w", err)
	}
	return tool.Result{Output: string(b)}, nil
}

// Transform inputs require read access on src and write access on dst. An
// in-place edit uses the overwrite policy; an empty dst uses framework scratch.
func checkTransformAccessDirect(ctx context.Context, agent *Agent, src, dst string) (string, string, error) {
	inPlace := dst != "" && dst == src
	resolvedSrc, err := agent.resolveFilePath(ctx, src, FileOperationRead)
	if err != nil {
		return "", "", err
	}
	if dst == "" {
		return resolvedSrc, "", nil
	}
	op := FileOperationWrite
	if inPlace {
		op = FileOperationOverwrite
		dst = resolvedSrc
	}
	resolvedDst, err := agent.resolveFilePath(ctx, dst, op)
	if err != nil {
		return "", "", err
	}
	return resolvedSrc, resolvedDst, nil
}

func convertLineEdits(in []capability.LineEditInput) ([]lineEdit, error) {
	if len(in) == 0 {
		return nil, errors.New("edits is required")
	}
	out := make([]lineEdit, 0, len(in))
	for i, e := range in {
		if e.Append != "" {
			out = append(out, lineEdit{isAppend: true, text: e.Append, hasText: true})
			continue
		}
		if e.From < 1 {
			return nil, fmt.Errorf("edit %d: `from` must be >= 1 (or set `append`)", i)
		}
		if e.Count < 0 {
			return nil, fmt.Errorf("edit %d: `count` must be >= 0", i)
		}
		le := lineEdit{from: e.From, count: e.Count}
		if e.Text != "" {
			le.text = e.Text
			le.hasText = true
		}
		if le.count == 0 && !le.hasText {
			return nil, fmt.Errorf("edit %d: an insert (count 0) needs `text`", i)
		}
		out = append(out, le)
	}
	return out, nil
}

func defaultStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
