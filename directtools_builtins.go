package agentsdk

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/airlockrun/goai/model"
	"github.com/airlockrun/goai/tool"
	"github.com/airlockrun/sol/websearch"
)

// Built-in fixed bindings, wrapped as direct LLM tools. Mirrors the
// gating + helper calls in vm.go's newVM — same access predicates,
// same agent.* / run.* Go helpers, only the envelope differs. Naming
// matches the JS binding (e.g. `fileRead`, `httpRequest`) so existing
// per-tool prompt guidance carries over and authors don't learn a
// second vocabulary.

// addBuiltinTools registers every fixed JS binding's direct-tool
// equivalent the run's caller is allowed to see. Skipped families
// (e.g. all file ops on a public run with no public-cap directories,
// or admin tools on a public run) leave a quieter surface than vm.go
// would have produced.
func addBuiltinTools(ts tool.Set, agent *Agent, run *run) {
	publicReadOK := agent.hasPublicDirCap(OpRead)
	publicWriteOK := agent.hasPublicDirCap(OpWrite)
	publicListOK := agent.hasPublicDirCap(OpList)
	authedFile := accessSatisfies(run.callerAccess, AccessUser)

	if authedFile || publicReadOK {
		ts["fileRead"] = wrapFileRead(agent, run)
		ts["fileReadBytes"] = wrapFileReadBytes(agent, run)
		ts["fileReadRangeBytes"] = wrapFileReadRangeBytes(agent, run)
		ts["fileGrep"] = wrapFileGrep(agent, run)
		ts["fileHead"] = wrapFileHead(agent, run)
		ts["fileTail"] = wrapFileTail(agent, run)
		ts["fileLines"] = wrapFileLines(agent, run)
		ts["fileStat"] = wrapFileStat(agent, run)
		ts["fileExists"] = wrapFileExists(agent, run)
		ts["fileShareURL"] = wrapFileShareURL(agent, run)
	}
	if authedFile {
		ts["fileEncode"] = wrapFileEncode(agent, run)
		ts["fileDecode"] = wrapFileDecode(agent, run)
		ts["fileDecodeText"] = wrapFileDecodeText(agent, run)
		ts["fileEditLines"] = wrapFileEditLines(agent, run)
		ts["fileSed"] = wrapFileSed(agent, run)
	}
	if authedFile || publicWriteOK {
		ts["fileWrite"] = wrapFileWrite(agent, run)
		ts["fileDelete"] = wrapFileDelete(agent, run)
	}
	if authedFile || publicListOK {
		ts["fileList"] = wrapFileList(agent, run)
	}

	ts["output"] = wrapOutput(agent, run)

	if accessSatisfies(run.callerAccess, AccessUser) {
		ts["httpRequest"] = wrapHTTPRequest(agent, run)
		ts["webSearch"] = wrapWebSearch(agent, run)
		ts["attachToContext"] = wrapAttachToContext(agent, run)
		ts["analyzeImage"] = wrapAnalyzeImage(agent, run)
		ts["transcribeAudio"] = wrapTranscribeAudio(agent, run)
		ts["generateImage"] = wrapGenerateImage(agent, run)
		ts["speak"] = wrapSpeak(agent, run)
		ts["embed"] = wrapEmbed(agent, run)
	}

	if accessSatisfies(run.callerAccess, AccessAdmin) {
		ts["queryDB"] = wrapQueryDB(agent, run)
		ts["execDB"] = wrapExecDB(agent, run)
		ts["requestUpgrade"] = wrapRequestUpgrade(agent, run)
	}
}

// --- file reads ---

type pathInput struct {
	Path string `json:"path" jsonschema:"description=Storage path (no leading slash). Use a path returned by another tool or one of the configured directories; never invent."`
}

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
			if err := agent.CheckFileAccess(ctx, in.Path, OpRead); err != nil {
				return tool.Result{}, err
			}
			rc, err := run.openCached(ctx, in.Path)
			if err != nil {
				return tool.Result{}, err
			}
			b, err := readCappedForJS(rc)
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
			if err := agent.CheckFileAccess(ctx, in.Path, OpRead); err != nil {
				return tool.Result{}, err
			}
			rc, err := run.openCached(ctx, in.Path)
			if err != nil {
				return tool.Result{}, err
			}
			b, err := readCappedForJS(rc)
			if err != nil {
				return tool.Result{}, err
			}
			return jsonResult(fileReadBytesOutput{Size: len(b), Base64: base64.StdEncoding.EncodeToString(b)})
		}).Build()
}

type fileReadRangeInput struct {
	Path   string `json:"path" jsonschema:"description=Storage path."`
	Start  int64  `json:"start" jsonschema:"description=Byte offset (0-based)."`
	Length int64  `json:"length" jsonschema:"description=Number of bytes to read."`
}

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
			if err := agent.CheckFileAccess(ctx, in.Path, OpRead); err != nil {
				return tool.Result{}, err
			}
			b, err := run.readRange(ctx, in.Path, in.Start, in.Length)
			if err != nil {
				return tool.Result{}, err
			}
			return jsonResult(fileReadBytesOutput{Size: len(b), Base64: base64.StdEncoding.EncodeToString(b)})
		}).Build()
}

type fileGrepInput struct {
	Path        string `json:"path" jsonschema:"description=Storage path."`
	Pattern     string `json:"pattern" jsonschema:"description=Regex pattern (RE2)."`
	IgnoreCase  bool   `json:"ignoreCase,omitempty"`
	Invert      bool   `json:"invert,omitempty"`
	LineNumbers bool   `json:"lineNumbers,omitempty"`
	Max         int    `json:"max,omitempty" jsonschema:"description=Cap matched lines; 0 = default."`
}

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
			if err := agent.CheckFileAccess(ctx, in.Path, OpRead); err != nil {
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

type fileNLinesInput struct {
	Path string `json:"path"`
	N    int    `json:"n,omitempty" jsonschema:"description=Line count; 0 = default 10."`
}

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
			if err := agent.CheckFileAccess(ctx, in.Path, OpRead); err != nil {
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
			if err := agent.CheckFileAccess(ctx, in.Path, OpRead); err != nil {
				return tool.Result{}, err
			}
			out, err := run.tailLines(ctx, in.Path, in.N)
			if err != nil {
				return tool.Result{}, err
			}
			return tool.Result{Output: out}, nil
		}).Build()
}

type fileLinesInput struct {
	Path  string `json:"path"`
	Start int    `json:"start,omitempty" jsonschema:"description=1-based line offset; 0 = default 1."`
	Count int    `json:"count,omitempty" jsonschema:"description=Line count; 0 = default 10."`
}

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
			if err := agent.CheckFileAccess(ctx, in.Path, OpRead); err != nil {
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
			if err := agent.CheckFileAccess(ctx, in.Path, OpRead); err != nil {
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
			if err := agent.CheckFileAccess(ctx, in.Path, OpRead); err != nil {
				return jsonResult(fileExistsOutput{Exists: false})
			}
			_, err := agent.StatFile(ctx, in.Path)
			return jsonResult(fileExistsOutput{Exists: err == nil})
		}).Build()
}

type fileShareURLInput struct {
	Path             string `json:"path"`
	ExpiresInMinutes int    `json:"expiresInMinutes,omitempty" jsonschema:"description=URL TTL; defaults to 60, capped at 1440 (24h)."`
}

func wrapFileShareURL(agent *Agent, run *run) tool.Tool {
	return tool.New("fileShareURL").
		Description("Mint a presigned, time-limited, unauthenticated URL for a stored file. Returns {url, expiresAtMs}. For embedding files in markdown or external one-off shares; for inline chat delivery prefer the `output` tool with type=\"file\".").
		SchemaFromStruct(fileShareURLInput{}).
		Execute(func(ctx context.Context, input json.RawMessage, opts tool.CallOptions) (tool.Result, error) {
			var in fileShareURLInput
			if err := json.Unmarshal(input, &in); err != nil {
				return tool.Result{}, err
			}
			ctx = run.checkedCtx()
			if err := agent.CheckFileAccess(ctx, in.Path, OpRead); err != nil {
				return tool.Result{}, err
			}
			ttl := time.Duration(in.ExpiresInMinutes) * time.Minute
			resp, err := agent.ShareFileURL(ctx, in.Path, ttl)
			if err != nil {
				return tool.Result{}, err
			}
			return jsonResult(resp)
		}).Build()
}

// --- file→file transforms (authed only) ---

type transformInput struct {
	Src   string `json:"src" jsonschema:"description=Source storage path."`
	Codec string `json:"codec" jsonschema:"description=Codec name (base64, base64url, hex, gzip; or a charset for fileDecodeText)."`
	Dst   string `json:"dst,omitempty" jsonschema:"description=Optional destination path. Omit for an auto scratch path. Must differ from src."`
}

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
			if err := checkTransformAccessDirect(ctx, agent, in.Src, in.Dst); err != nil {
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
			if err := checkTransformAccessDirect(ctx, agent, in.Src, in.Dst); err != nil {
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
			if err := checkTransformAccessDirect(ctx, agent, in.Src, in.Dst); err != nil {
				return tool.Result{}, err
			}
			res, err := run.transformFile(ctx, in.Src, in.Codec, in.Dst, "text/plain; charset=utf-8", ".txt", true, fn)
			if err != nil {
				return tool.Result{}, err
			}
			return jsonResult(res.toMap())
		}).Build()
}

type lineEditInput struct {
	From   int    `json:"from,omitempty" jsonschema:"description=1-based start line. Required unless 'append' is set."`
	Count  int    `json:"count,omitempty" jsonschema:"description=Lines from 'from' to operate on. 0 with text = insert before 'from'."`
	Text   string `json:"text,omitempty" jsonschema:"description=Replacement / insertion text."`
	Append string `json:"append,omitempty" jsonschema:"description=When set, append this text to the end of the file."`
}

type fileEditLinesInput struct {
	Src   string          `json:"src"`
	Edits []lineEditInput `json:"edits" jsonschema:"description=Edits applied in given order: {from,count,text}=replace · {from,count}=delete · {from,count:0,text}=insert before · {append}=append."`
	Dst   string          `json:"dst,omitempty" jsonschema:"description=Optional destination. Pass src to edit in place; omit for an auto scratch path."`
}

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
			if err := checkTransformAccessDirect(ctx, agent, in.Src, in.Dst); err != nil {
				return tool.Result{}, err
			}
			res, err := run.editLines(ctx, in.Src, in.Dst, edits)
			if err != nil {
				return tool.Result{}, err
			}
			return jsonResult(res.toMap())
		}).Build()
}

type fileSedInput struct {
	Src    string `json:"src"`
	Script string `json:"script" jsonschema:"description=sed subset: addresses N · N,M · /regex/ · $; commands s/re/repl/[gi] · d · c\\text · i\\text · a\\text. Replacement backrefs use Go syntax ($1)."`
	Dst    string `json:"dst,omitempty"`
}

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
			if err := checkTransformAccessDirect(ctx, agent, in.Src, in.Dst); err != nil {
				return tool.Result{}, err
			}
			res, err := run.sed(ctx, in.Src, in.Script, in.Dst)
			if err != nil {
				return tool.Result{}, err
			}
			return jsonResult(res.toMap())
		}).Build()
}

// --- file writes / delete / list ---

type fileWriteInput struct {
	Path        string `json:"path"`
	Data        string `json:"data" jsonschema:"description=UTF-8 text contents. For binary, set base64 instead."`
	Base64      string `json:"base64,omitempty" jsonschema:"description=Base64-encoded contents. Mutually exclusive with data."`
	ContentType string `json:"contentType,omitempty"`
}

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
			if err := agent.CheckFileAccess(ctx, in.Path, OpWrite); err != nil {
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
			if err := agent.CheckFileAccess(ctx, in.Path, OpWrite); err != nil {
				return tool.Result{}, err
			}
			if err := agent.DeleteFile(ctx, in.Path); err != nil {
				return tool.Result{}, err
			}
			run.invalidateCache(in.Path)
			return jsonResult(map[string]bool{"deleted": true})
		}).Build()
}

type fileListInput struct {
	Path      string `json:"path" jsonschema:"description=Directory path (trailing slash optional)."`
	Recursive bool   `json:"recursive,omitempty"`
}

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
			checkPath := strings.TrimRight(in.Path, "/")
			if checkPath == "" {
				checkPath = in.Path
			}
			if err := agent.CheckFileAccess(ctx, checkPath, OpList); err != nil {
				return tool.Result{}, err
			}
			files, err := agent.ListDir(ctx, in.Path, ListOpts{Recursive: in.Recursive})
			if err != nil {
				return tool.Result{}, err
			}
			return jsonResult(files)
		}).Build()
}

// --- output ---

type outputInput struct {
	Parts []DisplayPart `json:"parts" jsonschema:"description=Media parts. Each part is {type: image|file|audio|video, text?: caption, source?: storage path, url?, data?, filename?, mimeType?, alt?, duration?}. Prose goes in your normal reply — output is media-only."`
}

func wrapOutput(agent *Agent, run *run) tool.Tool {
	return tool.New("output").
		Description("Share media parts (image/file/audio/video) with the user / channel / calling sibling. Captions live on a media part's `text` field. Prose belongs in your normal reply, not here.").
		SchemaFromStruct(outputInput{}).
		Execute(func(ctx context.Context, input json.RawMessage, opts tool.CallOptions) (tool.Result, error) {
			var in outputInput
			if err := json.Unmarshal(input, &in); err != nil {
				return tool.Result{}, err
			}
			for i, p := range in.Parts {
				if p.Type == "text" {
					return tool.Result{}, fmt.Errorf("part %d is type=\"text\"; output is media-only — put prose in your normal reply, or set the `text` field on an image/file/audio/video part to use it as a caption", i)
				}
				if p.Type != "image" && p.Type != "file" && p.Type != "audio" && p.Type != "video" {
					return tool.Result{}, fmt.Errorf("part %d has unsupported type %q; expected image, file, audio, or video", i, p.Type)
				}
			}
			if err := run.output(run.ctx, in.Parts, ""); err != nil {
				return tool.Result{}, err
			}
			return jsonResult(map[string]int{"sent": len(in.Parts)})
		}).Build()
}

// --- http / web / vision / embed (authed) ---

type httpRequestInput struct {
	URL        string            `json:"url"`
	Method     string            `json:"method,omitempty" jsonschema:"description=Defaults to GET."`
	Headers    map[string]string `json:"headers,omitempty"`
	Body       string            `json:"body,omitempty" jsonschema:"description=Request body. JSON-encode objects yourself; Content-Type is not auto-set."`
	Timeout    int               `json:"timeout,omitempty"`
	SaveAs     string            `json:"saveAs,omitempty" jsonschema:"description=Storage path under a writable directory. When set, the response body is streamed to this path."`
	Raw        bool              `json:"raw,omitempty" jsonschema:"description=Skip HTML→markdown conversion (default is to convert HTML)."`
	AllHeaders bool              `json:"allHeaders,omitempty"`
}

func wrapHTTPRequest(agent *Agent, run *run) tool.Tool {
	httpClient := &proxyHTTPClient{client: agent.client}
	return tool.New("httpRequest").
		Description("HTTP request via Airlock's egress proxy. Returns {status, headers, contentType, size, body | (savedTo, bodyPreview, note)}. JSON responses are auto-parsed into `body`.").
		SchemaFromStruct(httpRequestInput{}).
		Execute(func(ctx context.Context, input json.RawMessage, opts tool.CallOptions) (tool.Result, error) {
			var in httpRequestInput
			if err := json.Unmarshal(input, &in); err != nil {
				return tool.Result{}, err
			}
			if in.URL == "" {
				return tool.Result{}, errors.New("url is required")
			}
			req := HTTPRequest{
				URL:        in.URL,
				Method:     defaultStr(in.Method, "GET"),
				Headers:    in.Headers,
				Body:       in.Body,
				Timeout:    in.Timeout,
				Raw:        in.Raw,
				AllHeaders: in.AllHeaders,
			}
			if in.SaveAs != "" {
				if err := agent.CheckFileAccess(run.checkedCtx(), in.SaveAs, OpWrite); err != nil {
					return tool.Result{}, fmt.Errorf("saveAs: %w", err)
				}
				req.SaveAs = in.SaveAs
			}
			resp, err := httpClient.Do(run.ctx, req)
			if err != nil {
				return tool.Result{}, err
			}
			return jsonResult(httpResponseToMap(resp))
		}).Build()
}

type webSearchInput struct {
	Query string `json:"query"`
	Count int    `json:"count,omitempty" jsonschema:"description=Default 5."`
}

func wrapWebSearch(agent *Agent, run *run) tool.Tool {
	searchClient := &proxySearchClient{client: agent.client}
	return tool.New("webSearch").
		Description("Search the web. Returns provider results; shape mirrors the airlock /search proxy.").
		SchemaFromStruct(webSearchInput{}).
		Execute(func(ctx context.Context, input json.RawMessage, opts tool.CallOptions) (tool.Result, error) {
			var in webSearchInput
			if err := json.Unmarshal(input, &in); err != nil {
				return tool.Result{}, err
			}
			count := in.Count
			if count == 0 {
				count = 5
			}
			resp, err := searchClient.Search(run.ctx, websearch.Request{Query: in.Query, Count: count})
			if err != nil {
				return tool.Result{}, err
			}
			return jsonResult(resp)
		}).Build()
}

func wrapAttachToContext(agent *Agent, run *run) tool.Tool {
	return tool.New("attachToContext").
		Description("Load a stored file (image/PDF/etc.) into your visual context for the next LLM turn. Idempotent per run. Returns a status string.").
		SchemaFromStruct(pathInput{}).
		Execute(func(ctx context.Context, input json.RawMessage, opts tool.CallOptions) (tool.Result, error) {
			var in pathInput
			if err := json.Unmarshal(input, &in); err != nil {
				return tool.Result{}, err
			}
			ctx = run.checkedCtx()
			if err := agent.CheckFileAccess(ctx, in.Path, OpRead); err != nil {
				return tool.Result{}, err
			}
			run.mu.Lock()
			if run.attachedKeys == nil {
				run.attachedKeys = make(map[string]struct{})
			}
			if _, ok := run.attachedKeys[in.Path]; ok {
				run.mu.Unlock()
				return tool.Result{Output: "Already in context for this turn."}, nil
			}
			run.attachedKeys[in.Path] = struct{}{}
			run.mu.Unlock()
			info, err := agent.StatFile(ctx, in.Path)
			if err != nil {
				return tool.Result{}, fmt.Errorf("file not found: %w", err)
			}
			if len(run.supportedModalities) > 0 && !mimeMatchesModalities(info.ContentType, run.supportedModalities) {
				return tool.Result{}, fmt.Errorf(
					"%s files are not supported by the current model. Supported types: %s. Use fileRead(path) for text-based files",
					info.ContentType, strings.Join(run.supportedModalities, ", "))
			}
			run.mu.Lock()
			run.pendingAttachments = append(run.pendingAttachments, tool.Attachment{
				Data:     "s3ref:" + in.Path,
				MimeType: info.ContentType,
				Filename: pathBase(in.Path),
			})
			attachments := append([]tool.Attachment(nil), run.pendingAttachments...)
			run.pendingAttachments = nil
			run.mu.Unlock()
			return tool.Result{
				Output:      fmt.Sprintf("Attached %s (%s). The file is visible on the next turn.", in.Path, info.ContentType),
				Attachments: attachments,
			}, nil
		}).Build()
}

type analyzeImageInput struct {
	Path     string `json:"path"`
	Question string `json:"question,omitempty"`
}

func wrapAnalyzeImage(agent *Agent, run *run) tool.Tool {
	return tool.New("analyzeImage").
		Description("Ask the platform's vision model about a stored image (works even if your chat model has no vision). Returns the model's text reply.").
		SchemaFromStruct(analyzeImageInput{}).
		Execute(func(ctx context.Context, input json.RawMessage, opts tool.CallOptions) (tool.Result, error) {
			var in analyzeImageInput
			if err := json.Unmarshal(input, &in); err != nil {
				return tool.Result{}, err
			}
			ctx = run.checkedCtx()
			if err := agent.CheckFileAccess(ctx, in.Path, OpRead); err != nil {
				return tool.Result{}, err
			}
			text, err := run.analyzeImage(ctx, in.Path, in.Question)
			if err != nil {
				return tool.Result{}, err
			}
			return tool.Result{Output: text}, nil
		}).Build()
}

type transcribeAudioInput struct {
	Path     string `json:"path"`
	Language string `json:"language,omitempty" jsonschema:"description=ISO-639 hint, optional."`
	Prompt   string `json:"prompt,omitempty" jsonschema:"description=Optional prior-context hint for the transcriber."`
}

func wrapTranscribeAudio(agent *Agent, run *run) tool.Tool {
	return tool.New("transcribeAudio").
		Description("Transcribe a stored audio file via the platform's transcription model. Returns {text, language?, duration?}.").
		SchemaFromStruct(transcribeAudioInput{}).
		Execute(func(ctx context.Context, input json.RawMessage, opts tool.CallOptions) (tool.Result, error) {
			var in transcribeAudioInput
			if err := json.Unmarshal(input, &in); err != nil {
				return tool.Result{}, err
			}
			ctx = run.checkedCtx()
			if err := agent.CheckFileAccess(ctx, in.Path, OpRead); err != nil {
				return tool.Result{}, err
			}
			res, err := run.transcribeAudio(ctx, in.Path, model.TranscribeCallOptions{
				Language: in.Language,
				Prompt:   in.Prompt,
			})
			if err != nil {
				return tool.Result{}, err
			}
			out := map[string]any{"text": res.Text}
			if res.Language != "" {
				out["language"] = res.Language
			}
			if res.Duration != nil {
				out["duration"] = *res.Duration
			}
			return jsonResult(out)
		}).Build()
}

type generateImageInput struct {
	Prompt      string `json:"prompt"`
	SaveAs      string `json:"saveAs,omitempty" jsonschema:"description=Storage path; defaults to an auto scratch path."`
	Size        string `json:"size,omitempty"`
	AspectRatio string `json:"aspectRatio,omitempty"`
	Seed        *int64 `json:"seed,omitempty"`
}

func wrapGenerateImage(agent *Agent, run *run) tool.Tool {
	return tool.New("generateImage").
		Description("Generate an image from a text prompt via the platform's image model. Returns {path, mimeType, size}.").
		SchemaFromStruct(generateImageInput{}).
		Execute(func(ctx context.Context, input json.RawMessage, opts tool.CallOptions) (tool.Result, error) {
			var in generateImageInput
			if err := json.Unmarshal(input, &in); err != nil {
				return tool.Result{}, err
			}
			if in.Prompt == "" {
				return tool.Result{}, errors.New("prompt is required")
			}
			ctx = run.checkedCtx()
			if in.SaveAs != "" {
				if err := agent.CheckFileAccess(ctx, in.SaveAs, OpWrite); err != nil {
					return tool.Result{}, fmt.Errorf("saveAs: %w", err)
				}
			}
			res, err := run.generateImage(run.ctx, in.Prompt, in.SaveAs, model.ImageCallOptions{
				Size:        in.Size,
				AspectRatio: in.AspectRatio,
				Seed:        in.Seed,
			})
			if err != nil {
				return tool.Result{}, err
			}
			return jsonResult(res.toMap())
		}).Build()
}

type speakInput struct {
	Text         string   `json:"text"`
	SaveAs       string   `json:"saveAs,omitempty"`
	Voice        string   `json:"voice,omitempty"`
	OutputFormat string   `json:"outputFormat,omitempty"`
	Speed        *float64 `json:"speed,omitempty"`
}

func wrapSpeak(agent *Agent, run *run) tool.Tool {
	return tool.New("speak").
		Description("Synthesize audio from text via the platform's TTS model. Returns {path, mimeType, size}.").
		SchemaFromStruct(speakInput{}).
		Execute(func(ctx context.Context, input json.RawMessage, opts tool.CallOptions) (tool.Result, error) {
			var in speakInput
			if err := json.Unmarshal(input, &in); err != nil {
				return tool.Result{}, err
			}
			if in.Text == "" {
				return tool.Result{}, errors.New("text is required")
			}
			ctx = run.checkedCtx()
			if in.SaveAs != "" {
				if err := agent.CheckFileAccess(ctx, in.SaveAs, OpWrite); err != nil {
					return tool.Result{}, fmt.Errorf("saveAs: %w", err)
				}
			}
			res, err := run.generateSpeech(run.ctx, in.Text, in.SaveAs, model.SpeechCallOptions{
				Voice:        in.Voice,
				OutputFormat: in.OutputFormat,
				Speed:        in.Speed,
			})
			if err != nil {
				return tool.Result{}, err
			}
			return jsonResult(res.toMap())
		}).Build()
}

type embedInput struct {
	Text  string   `json:"text,omitempty" jsonschema:"description=A single text input. Mutually exclusive with 'texts'."`
	Texts []string `json:"texts,omitempty"`
}

func wrapEmbed(agent *Agent, run *run) tool.Tool {
	return tool.New("embed").
		Description("Compute embeddings via the platform's embedding model. Pass `text` for one input or `texts` for batch. Returns {vectors, model, dimensions}.").
		SchemaFromStruct(embedInput{}).
		Execute(func(ctx context.Context, input json.RawMessage, opts tool.CallOptions) (tool.Result, error) {
			var in embedInput
			if err := json.Unmarshal(input, &in); err != nil {
				return tool.Result{}, err
			}
			var batch []string
			switch {
			case len(in.Texts) > 0:
				batch = in.Texts
			case in.Text != "":
				batch = []string{in.Text}
			default:
				return tool.Result{}, errors.New("`text` or `texts` is required")
			}
			res, err := run.embed(run.checkedCtx(), batch)
			if err != nil {
				return tool.Result{}, err
			}
			return jsonResult(res)
		}).Build()
}

// --- admin (queryDB / execDB / requestUpgrade) ---

type queryDBInput struct {
	SQL    string `json:"sql"`
	Params []any  `json:"params,omitempty" jsonschema:"description=Positional parameters bound as $1, $2, ..."`
}

func wrapQueryDB(agent *Agent, run *run) tool.Tool {
	return tool.New("queryDB").
		Description("Run a read-only SQL query against the agent's database. Returns an array of row objects.").
		SchemaFromStruct(queryDBInput{}).
		Execute(func(ctx context.Context, input json.RawMessage, opts tool.CallOptions) (tool.Result, error) {
			var in queryDBInput
			if err := json.Unmarshal(input, &in); err != nil {
				return tool.Result{}, err
			}
			db := agent.DB()
			if db == nil {
				return tool.Result{}, errors.New("agent database not configured (AIRLOCK_DB_URL not set)")
			}
			rows, err := db.QueryContext(run.ctx, in.SQL, in.Params...)
			if err != nil {
				return tool.Result{}, err
			}
			defer rows.Close()
			res, err := rowsToMaps(rows)
			if err != nil {
				return tool.Result{}, err
			}
			return jsonResult(res)
		}).Build()
}

type execDBInput struct {
	SQL    string `json:"sql"`
	Params []any  `json:"params,omitempty"`
}

type execDBOutput struct {
	RowsAffected int64 `json:"rowsAffected"`
}

func wrapExecDB(agent *Agent, run *run) tool.Tool {
	return tool.New("execDB").
		Description("Execute a SQL statement (DDL/DML) against the agent's database. Returns {rowsAffected}.").
		SchemaFromStruct(execDBInput{}).
		Execute(func(ctx context.Context, input json.RawMessage, opts tool.CallOptions) (tool.Result, error) {
			var in execDBInput
			if err := json.Unmarshal(input, &in); err != nil {
				return tool.Result{}, err
			}
			db := agent.DB()
			if db == nil {
				return tool.Result{}, errors.New("agent database not configured (AIRLOCK_DB_URL not set)")
			}
			res, err := db.ExecContext(run.ctx, in.SQL, in.Params...)
			if err != nil {
				return tool.Result{}, err
			}
			affected, _ := res.RowsAffected()
			return jsonResult(execDBOutput{RowsAffected: affected})
		}).Build()
}

type requestUpgradeInput struct {
	Description string `json:"description"`
}

func wrapRequestUpgrade(agent *Agent, run *run) tool.Tool {
	return tool.New("requestUpgrade").
		Description("Request a platform-side upgrade (new connection / tool / route / etc.) be built and deployed. The agent keeps running until the new build finishes.").
		SchemaFromStruct(requestUpgradeInput{}).
		Execute(func(ctx context.Context, input json.RawMessage, opts tool.CallOptions) (tool.Result, error) {
			var in requestUpgradeInput
			if err := json.Unmarshal(input, &in); err != nil {
				return tool.Result{}, err
			}
			if in.Description == "" {
				return tool.Result{}, errors.New("description is required")
			}
			body := struct {
				Description    string `json:"description"`
				ConversationID string `json:"conversationId,omitempty"`
			}{in.Description, run.conversationID}
			if err := agent.client.doJSON(run.ctx, "POST", "/api/agent/upgrade", body, nil); err != nil {
				return tool.Result{}, err
			}
			return tool.Result{Output: "Upgrade requested. The agent will be regenerated in the background."}, nil
		}).Build()
}

// --- shared helpers ---

// jsonResult marshals v to JSON and wraps as a tool.Result.
func jsonResult(v any) (tool.Result, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return tool.Result{}, fmt.Errorf("encode tool output: %w", err)
	}
	return tool.Result{Output: string(b)}, nil
}

// checkTransformAccessDirect is the direct-mode counterpart to
// vm.go::checkTransformAccess — same gate (Read on src, Write on dst
// when set), without the goja error envelope. dst="" means an auto
// scratch path under tmp/, which inherits write cap from the tmp dir.
func checkTransformAccessDirect(ctx context.Context, agent *Agent, src, dst string) error {
	if err := agent.CheckFileAccess(ctx, src, OpRead); err != nil {
		return err
	}
	if dst == "" {
		return nil
	}
	if dst == src {
		// In-place edit: write cap on src covers it.
		return agent.CheckFileAccess(ctx, src, OpWrite)
	}
	return agent.CheckFileAccess(ctx, dst, OpWrite)
}

// convertLineEdits turns the direct-mode lineEditInput slice into the
// internal lineEdit shape parseLineEdits would have produced from a
// goja.Value. Same validation; just no goja.
func convertLineEdits(in []lineEditInput) ([]lineEdit, error) {
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
