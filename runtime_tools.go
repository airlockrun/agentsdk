package agentsdk

// App-owned capability executors share their schemas with capability.Catalog.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/airlockrun/agentsdk/capability"

	"github.com/airlockrun/goai/tool"
)

type pathInput = capability.PathInput

func wrapFileRead(agent *Agent, run *run) tool.Tool {
	return tool.New("fileRead").
		Description("Read a stored file as UTF-8 text. Capped at 16 MiB; for binary use fileReadBytes; for large files use fileHead/fileTail/fileGrep/fileLines/fileReadRangeBytes.").
		SchemaFromStruct(pathInput{}).
		Execute(func(ctx context.Context, input json.RawMessage, opts tool.CallOptions) (tool.Result, error) {
			var in pathInput
			if err := json.Unmarshal(input, &in); err != nil {
				return tool.Result{}, err
			}
			ctx = run.checkedCtx()
			var err error
			in.Path, err = agent.resolveFilePath(ctx, in.Path, FileOperationRead)
			if err != nil {
				return tool.Result{}, err
			}
			rc, err := run.openCached(ctx, in.Path)
			if err != nil {
				return tool.Result{}, err
			}
			b, err := readCappedFile(rc)
			if err != nil {
				return tool.Result{}, err
			}
			return tool.Result{Output: string(b)}, nil
		}).Build()
}

type fileReadBytesOutput struct {
	Size   int    `json:"size"`
	Base64 string `json:"base64"`
}

func wrapFileReadBytes(agent *Agent, run *run) tool.Tool {
	return tool.New("fileReadBytes").
		Description("Read a stored file's raw bytes; returns {size, base64}. Capped at 16 MiB.").
		SchemaFromStruct(pathInput{}).
		Execute(func(ctx context.Context, input json.RawMessage, opts tool.CallOptions) (tool.Result, error) {
			var in pathInput
			if err := json.Unmarshal(input, &in); err != nil {
				return tool.Result{}, err
			}
			ctx = run.checkedCtx()
			var err error
			in.Path, err = agent.resolveFilePath(ctx, in.Path, FileOperationRead)
			if err != nil {
				return tool.Result{}, err
			}
			rc, err := run.openCached(ctx, in.Path)
			if err != nil {
				return tool.Result{}, err
			}
			b, err := readCappedFile(rc)
			if err != nil {
				return tool.Result{}, err
			}
			return jsonResult(fileReadBytesOutput{Size: len(b), Base64: base64.StdEncoding.EncodeToString(b)})
		}).Build()
}

type fileReadRangeInput = capability.FileReadRangeInput

func wrapFileReadRangeBytes(agent *Agent, run *run) tool.Tool {
	return tool.New("fileReadRangeBytes").
		Description("Read an exact byte window from a stored file; returns {size, base64}. Cache-aware Range read.").
		SchemaFromStruct(fileReadRangeInput{}).
		Execute(func(ctx context.Context, input json.RawMessage, opts tool.CallOptions) (tool.Result, error) {
			var in fileReadRangeInput
			if err := json.Unmarshal(input, &in); err != nil {
				return tool.Result{}, err
			}
			ctx = run.checkedCtx()
			var err error
			in.Path, err = agent.resolveFilePath(ctx, in.Path, FileOperationRead)
			if err != nil {
				return tool.Result{}, err
			}
			b, err := run.readRange(ctx, in.Path, in.Start, in.Length)
			if err != nil {
				return tool.Result{}, err
			}
			return jsonResult(fileReadBytesOutput{Size: len(b), Base64: base64.StdEncoding.EncodeToString(b)})
		}).Build()
}

type fileGrepInput = capability.FileGrepInput

func wrapFileGrep(agent *Agent, run *run) tool.Tool {
	return tool.New("fileGrep").
		Description("Stream-grep a file with a regex. Returns matching lines as text. Output is bounded; reports how many matches were dropped past the cap.").
		SchemaFromStruct(fileGrepInput{}).
		Execute(func(ctx context.Context, input json.RawMessage, opts tool.CallOptions) (tool.Result, error) {
			var in fileGrepInput
			if err := json.Unmarshal(input, &in); err != nil {
				return tool.Result{}, err
			}
			if in.Pattern == "" {
				return tool.Result{}, errors.New("pattern is required")
			}
			ctx = run.checkedCtx()
			var err error
			in.Path, err = agent.resolveFilePath(ctx, in.Path, FileOperationRead)
			if err != nil {
				return tool.Result{}, err
			}
			out, err := run.grepFile(ctx, in.Path, in.Pattern, grepOpts{
				ignoreCase:  in.IgnoreCase,
				invert:      in.Invert,
				lineNumbers: in.LineNumbers,
				max:         in.Max,
			})
			if err != nil {
				return tool.Result{}, err
			}
			return tool.Result{Output: out}, nil
		}).Build()
}

type fileNLinesInput = capability.FileNLinesInput

func wrapFileHead(agent *Agent, run *run) tool.Tool {
	return tool.New("fileHead").
		Description("Read the first N lines of a stored text file (default 10).").
		SchemaFromStruct(fileNLinesInput{}).
		Execute(func(ctx context.Context, input json.RawMessage, opts tool.CallOptions) (tool.Result, error) {
			var in fileNLinesInput
			if err := json.Unmarshal(input, &in); err != nil {
				return tool.Result{}, err
			}
			ctx = run.checkedCtx()
			var err error
			in.Path, err = agent.resolveFilePath(ctx, in.Path, FileOperationRead)
			if err != nil {
				return tool.Result{}, err
			}
			out, err := run.headLines(ctx, in.Path, in.N)
			if err != nil {
				return tool.Result{}, err
			}
			return tool.Result{Output: out}, nil
		}).Build()
}

func wrapFileTail(agent *Agent, run *run) tool.Tool {
	return tool.New("fileTail").
		Description("Read the last N lines of a stored text file (default 10). Fetches only the trailing window — safe for large files.").
		SchemaFromStruct(fileNLinesInput{}).
		Execute(func(ctx context.Context, input json.RawMessage, opts tool.CallOptions) (tool.Result, error) {
			var in fileNLinesInput
			if err := json.Unmarshal(input, &in); err != nil {
				return tool.Result{}, err
			}
			ctx = run.checkedCtx()
			var err error
			in.Path, err = agent.resolveFilePath(ctx, in.Path, FileOperationRead)
			if err != nil {
				return tool.Result{}, err
			}
			out, err := run.tailLines(ctx, in.Path, in.N)
			if err != nil {
				return tool.Result{}, err
			}
			return tool.Result{Output: out}, nil
		}).Build()
}

type fileLinesInput = capability.FileLinesInput

func wrapFileLines(agent *Agent, run *run) tool.Tool {
	return tool.New("fileLines").
		Description("Read a line window from a text file starting at the 1-based line `start` for `count` lines.").
		SchemaFromStruct(fileLinesInput{}).
		Execute(func(ctx context.Context, input json.RawMessage, opts tool.CallOptions) (tool.Result, error) {
			var in fileLinesInput
			if err := json.Unmarshal(input, &in); err != nil {
				return tool.Result{}, err
			}
			ctx = run.checkedCtx()
			var err error
			in.Path, err = agent.resolveFilePath(ctx, in.Path, FileOperationRead)
			if err != nil {
				return tool.Result{}, err
			}
			out, err := run.readLineWindow(ctx, in.Path, in.Start, in.Count)
			if err != nil {
				return tool.Result{}, err
			}
			return tool.Result{Output: out}, nil
		}).Build()
}

func wrapFileStat(agent *Agent, run *run) tool.Tool {
	return tool.New("fileStat").
		Description("Return metadata for a stored file: {path, filename, contentType, size, lastModified}.").
		SchemaFromStruct(pathInput{}).
		Execute(func(ctx context.Context, input json.RawMessage, opts tool.CallOptions) (tool.Result, error) {
			var in pathInput
			if err := json.Unmarshal(input, &in); err != nil {
				return tool.Result{}, err
			}
			ctx = run.checkedCtx()
			var err error
			in.Path, err = agent.resolveFilePath(ctx, in.Path, FileOperationRead)
			if err != nil {
				return tool.Result{}, err
			}
			info, err := agent.StatFile(ctx, in.Path)
			if err != nil {
				return tool.Result{}, err
			}
			return jsonResult(info)
		}).Build()
}

type fileExistsOutput struct {
	Exists bool `json:"exists"`
}

func wrapFileExists(agent *Agent, run *run) tool.Tool {
	return tool.New("fileExists").
		Description("Check whether a stored file exists. Returns {exists}.").
		SchemaFromStruct(pathInput{}).
		Execute(func(ctx context.Context, input json.RawMessage, opts tool.CallOptions) (tool.Result, error) {
			var in pathInput
			if err := json.Unmarshal(input, &in); err != nil {
				return tool.Result{}, err
			}
			ctx = run.checkedCtx()
			// Indistinguishable from "not found" by design: access denial
			// must not leak whether the file exists.
			resolved, err := agent.resolveFilePath(ctx, in.Path, FileOperationRead)
			if err != nil {
				if errors.Is(err, ErrNotFound) {
					return jsonResult(fileExistsOutput{Exists: false})
				}
				return tool.Result{}, err
			}
			_, err = agent.StatFile(ctx, resolved)
			return jsonResult(fileExistsOutput{Exists: err == nil})
		}).Build()
}

type transformInput = capability.TransformInput

func wrapFileEncode(agent *Agent, run *run) tool.Tool {
	return tool.New("fileEncode").
		Description("Encode a file with a codec (base64, base64url, hex, gzip). Returns {inline, content?, savedTo?, preview?, size}. Omit dst for an auto scratch path.").
		SchemaFromStruct(transformInput{}).
		Execute(func(ctx context.Context, input json.RawMessage, opts tool.CallOptions) (tool.Result, error) {
			var in transformInput
			if err := json.Unmarshal(input, &in); err != nil {
				return tool.Result{}, err
			}
			fn, ok := encoders[in.Codec]
			if !ok {
				return tool.Result{}, fmt.Errorf("unknown codec %q (base64, base64url, hex, gzip)", in.Codec)
			}
			ctx = run.checkedCtx()
			var err error
			in.Src, in.Dst, err = checkTransformAccessDirect(ctx, agent, in.Src, in.Dst)
			if err != nil {
				return tool.Result{}, err
			}
			res, err := run.transformFile(ctx, in.Src, in.Codec, in.Dst, encodeContentType(in.Codec), codecSuffix[in.Codec], textCodecs[in.Codec], fn)
			if err != nil {
				return tool.Result{}, err
			}
			return jsonResult(res.toMap())
		}).Build()
}

func wrapFileDecode(agent *Agent, run *run) tool.Tool {
	return tool.New("fileDecode").
		Description("Decode a file from a codec (base64, base64url, hex, gzip). Same return shape as fileEncode.").
		SchemaFromStruct(transformInput{}).
		Execute(func(ctx context.Context, input json.RawMessage, opts tool.CallOptions) (tool.Result, error) {
			var in transformInput
			if err := json.Unmarshal(input, &in); err != nil {
				return tool.Result{}, err
			}
			fn, ok := decoders[in.Codec]
			if !ok {
				return tool.Result{}, fmt.Errorf("unknown codec %q (base64, base64url, hex, gzip)", in.Codec)
			}
			ctx = run.checkedCtx()
			var err error
			in.Src, in.Dst, err = checkTransformAccessDirect(ctx, agent, in.Src, in.Dst)
			if err != nil {
				return tool.Result{}, err
			}
			res, err := run.transformFile(ctx, in.Src, in.Codec, in.Dst, "application/octet-stream", ".bin", false, fn)
			if err != nil {
				return tool.Result{}, err
			}
			return jsonResult(res.toMap())
		}).Build()
}

func wrapFileDecodeText(agent *Agent, run *run) tool.Tool {
	return tool.New("fileDecodeText").
		Description("Decode bytes in a non-UTF-8 charset (latin1, utf-16, ...) to UTF-8 text. `codec` is the charset name. Same return shape as fileEncode.").
		SchemaFromStruct(transformInput{}).
		Execute(func(ctx context.Context, input json.RawMessage, opts tool.CallOptions) (tool.Result, error) {
			var in transformInput
			if err := json.Unmarshal(input, &in); err != nil {
				return tool.Result{}, err
			}
			fn, err := lookupCharset(in.Codec)
			if err != nil {
				return tool.Result{}, err
			}
			ctx = run.checkedCtx()
			in.Src, in.Dst, err = checkTransformAccessDirect(ctx, agent, in.Src, in.Dst)
			if err != nil {
				return tool.Result{}, err
			}
			res, err := run.transformFile(ctx, in.Src, in.Codec, in.Dst, "text/plain; charset=utf-8", ".txt", true, fn)
			if err != nil {
				return tool.Result{}, err
			}
			return jsonResult(res.toMap())
		}).Build()
}

type fileEditLinesInput = capability.FileEditLinesInput

func wrapFileEditLines(agent *Agent, run *run) tool.Tool {
	return tool.New("fileEditLines").
		Description("Apply 1-based line-addressed edits to a file. Streaming; safe for large files. Returns the same {inline, content?, savedTo?, preview?, size} shape as fileEncode.").
		SchemaFromStruct(fileEditLinesInput{}).
		Execute(func(ctx context.Context, input json.RawMessage, opts tool.CallOptions) (tool.Result, error) {
			var in fileEditLinesInput
			if err := json.Unmarshal(input, &in); err != nil {
				return tool.Result{}, err
			}
			edits, err := convertLineEdits(in.Edits)
			if err != nil {
				return tool.Result{}, err
			}
			ctx = run.checkedCtx()
			in.Src, in.Dst, err = checkTransformAccessDirect(ctx, agent, in.Src, in.Dst)
			if err != nil {
				return tool.Result{}, err
			}
			res, err := run.editLines(ctx, in.Src, in.Dst, edits)
			if err != nil {
				return tool.Result{}, err
			}
			return jsonResult(res.toMap())
		}).Build()
}

type fileSedInput = capability.FileSedInput

func wrapFileSed(agent *Agent, run *run) tool.Tool {
	return tool.New("fileSed").
		Description("Apply a sed-subset script to a file. Streaming. Same return shape as fileEncode.").
		SchemaFromStruct(fileSedInput{}).
		Execute(func(ctx context.Context, input json.RawMessage, opts tool.CallOptions) (tool.Result, error) {
			var in fileSedInput
			if err := json.Unmarshal(input, &in); err != nil {
				return tool.Result{}, err
			}
			if in.Script == "" {
				return tool.Result{}, errors.New("script is required")
			}
			ctx = run.checkedCtx()
			var err error
			in.Src, in.Dst, err = checkTransformAccessDirect(ctx, agent, in.Src, in.Dst)
			if err != nil {
				return tool.Result{}, err
			}
			res, err := run.sed(ctx, in.Src, in.Script, in.Dst)
			if err != nil {
				return tool.Result{}, err
			}
			return jsonResult(res.toMap())
		}).Build()
}

type fileWriteInput = capability.FileWriteInput

func wrapFileWrite(agent *Agent, run *run) tool.Tool {
	return tool.New("fileWrite").
		Description("Write a stored file. For text content set `data`; for binary set `base64`. Returns FileInfo {path, filename, contentType, size, lastModified}.").
		SchemaFromStruct(fileWriteInput{}).
		Execute(func(ctx context.Context, input json.RawMessage, opts tool.CallOptions) (tool.Result, error) {
			var in fileWriteInput
			if err := json.Unmarshal(input, &in); err != nil {
				return tool.Result{}, err
			}
			var body []byte
			switch {
			case in.Base64 != "":
				b, err := base64.StdEncoding.DecodeString(in.Base64)
				if err != nil {
					return tool.Result{}, fmt.Errorf("decode base64: %w", err)
				}
				body = b
			case in.Data != "":
				body = []byte(in.Data)
			default:
				return tool.Result{}, errors.New("data or base64 is required")
			}
			ctx = run.checkedCtx()
			var err error
			in.Path, err = agent.resolveFilePath(ctx, in.Path, FileOperationWrite)
			if err != nil {
				return tool.Result{}, err
			}
			info, err := agent.WriteFile(ctx, in.Path, strings.NewReader(string(body)), in.ContentType)
			if err != nil {
				return tool.Result{}, err
			}
			run.invalidateCache(string(info.Path))
			return jsonResult(info)
		}).Build()
}

func wrapFileDelete(agent *Agent, run *run) tool.Tool {
	return tool.New("fileDelete").
		Description("Delete a stored file. No-op result on success.").
		SchemaFromStruct(pathInput{}).
		Execute(func(ctx context.Context, input json.RawMessage, opts tool.CallOptions) (tool.Result, error) {
			var in pathInput
			if err := json.Unmarshal(input, &in); err != nil {
				return tool.Result{}, err
			}
			ctx = run.checkedCtx()
			var err error
			in.Path, err = agent.resolveFilePath(ctx, in.Path, FileOperationDelete)
			if err != nil {
				return tool.Result{}, err
			}
			if err := agent.DeleteFile(ctx, in.Path); err != nil {
				return tool.Result{}, err
			}
			run.invalidateCache(in.Path)
			return jsonResult(map[string]bool{"deleted": true})
		}).Build()
}

type fileListInput = capability.FileListInput

func wrapFileList(agent *Agent, run *run) tool.Tool {
	return tool.New("fileList").
		Description("List entries under a storage path. Returns FileInfo[]. `recursive` walks subdirectories.").
		SchemaFromStruct(fileListInput{}).
		Execute(func(ctx context.Context, input json.RawMessage, opts tool.CallOptions) (tool.Result, error) {
			var in fileListInput
			if err := json.Unmarshal(input, &in); err != nil {
				return tool.Result{}, err
			}
			ctx = run.checkedCtx()
			var err error
			in.Path, err = agent.resolveFilePath(ctx, in.Path, FileOperationList)
			if err != nil {
				return tool.Result{}, err
			}
			files, err := agent.ListDir(ctx, in.Path, ListOpts{Recursive: in.Recursive})
			if err != nil {
				return tool.Result{}, err
			}
			return jsonResult(files)
		}).Build()
}

type queryDBInput = capability.QueryDBInput

func wrapQueryDB(agent *Agent, run *run) tool.Tool {
	return tool.New("queryDB").
		Description("Run a read-only SQL query against the agent's database. Runs inside a read-only transaction, so writes (INSERT/UPDATE/DELETE/DDL) are rejected. Returns an array of row objects.").
		SchemaFromStruct(queryDBInput{}).
		Execute(func(ctx context.Context, input json.RawMessage, opts tool.CallOptions) (tool.Result, error) {
			var in queryDBInput
			if err := json.Unmarshal(input, &in); err != nil {
				return tool.Result{}, err
			}
			db := agent.DB()
			res, err := queryReadOnly(run.ctx, db, in.SQL, in.Params...)
			if err != nil {
				return tool.Result{}, err
			}
			return jsonResult(res)
		}).Build()
}
