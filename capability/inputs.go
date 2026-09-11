package capability

import "github.com/airlockrun/agentsdk/wire"

// These input contracts are shared by catalogue schemas and app executors.
type PathInput struct {
	Path string `json:"path" jsonschema:"description=Storage path (no leading slash). Use a path returned by another tool or one of the configured directories; never invent."`
}

type FileReadRangeInput struct {
	Path   string `json:"path" jsonschema:"description=Storage path."`
	Start  int64  `json:"start" jsonschema:"description=Byte offset (0-based)."`
	Length int64  `json:"length" jsonschema:"description=Number of bytes to read."`
}

type FileGrepInput struct {
	Path        string `json:"path" jsonschema:"description=Storage path."`
	Pattern     string `json:"pattern" jsonschema:"description=Regex pattern (RE2)."`
	IgnoreCase  bool   `json:"ignoreCase,omitempty"`
	Invert      bool   `json:"invert,omitempty"`
	LineNumbers bool   `json:"lineNumbers,omitempty"`
	Max         int    `json:"max,omitempty" jsonschema:"description=Cap matched lines; 0 = default."`
}

type FileNLinesInput struct {
	Path string `json:"path"`
	N    int    `json:"n,omitempty" jsonschema:"description=Line count; 0 = default 10."`
}

type FileLinesInput struct {
	Path  string `json:"path"`
	Start int    `json:"start,omitempty" jsonschema:"description=1-based line offset; 0 = default 1."`
	Count int    `json:"count,omitempty" jsonschema:"description=Line count; 0 = default 10."`
}

type FileShareURLInput struct {
	Path             string `json:"path"`
	ExpiresInMinutes int    `json:"expiresInMinutes,omitempty" jsonschema:"description=URL TTL; defaults to 60, capped at 1440 (24h)."`
}

type TransformInput struct {
	Src   string `json:"src" jsonschema:"description=Source storage path."`
	Codec string `json:"codec" jsonschema:"description=Codec name (base64, base64url, hex, gzip; or a charset for fileDecodeText)."`
	Dst   string `json:"dst,omitempty" jsonschema:"description=Optional destination path. Omit for an auto scratch path. Must differ from src."`
}

type LineEditInput struct {
	From   int    `json:"from,omitempty" jsonschema:"description=1-based start line. Required unless 'append' is set."`
	Count  int    `json:"count,omitempty" jsonschema:"description=Lines from 'from' to operate on. 0 with text = insert before 'from'."`
	Text   string `json:"text,omitempty" jsonschema:"description=Replacement / insertion text."`
	Append string `json:"append,omitempty" jsonschema:"description=When set, append this text to the end of the file."`
}

type FileEditLinesInput struct {
	Src   string          `json:"src"`
	Edits []LineEditInput `json:"edits" jsonschema:"description=Edits applied in given order: from/count/text replaces; from/count deletes; from/count:0/text inserts; append appends."`
	Dst   string          `json:"dst,omitempty" jsonschema:"description=Optional destination. Pass src to edit in place; omit for an auto scratch path."`
}

type FileSedInput struct {
	Src    string `json:"src"`
	Script string `json:"script" jsonschema:"description=sed subset: addresses N, N through M, /regex/, $; commands s/re/repl/[gi], d, c\\text, i\\text, a\\text. Replacement backrefs use Go syntax ($1)."`
	Dst    string `json:"dst,omitempty"`
}

type FileWriteInput struct {
	Path        string `json:"path"`
	Data        string `json:"data" jsonschema:"description=UTF-8 text contents. For binary, set base64 instead."`
	Base64      string `json:"base64,omitempty" jsonschema:"description=Base64-encoded contents. Mutually exclusive with data."`
	ContentType string `json:"contentType,omitempty"`
}

type FileListInput struct {
	Path      string `json:"path" jsonschema:"description=Directory path (trailing slash optional)."`
	Recursive bool   `json:"recursive,omitempty"`
}

type OutputInput struct {
	Parts []wire.DisplayPart `json:"parts" jsonschema:"description=Media parts. Each part is {type: image|file|audio|video, text?: caption, source?: storage path, url?, data?, filename?, mimeType?, alt?, duration?}. Prose goes in your normal reply; output is media-only."`
}

type HTTPRequestInput struct {
	URL        string            `json:"url"`
	Method     string            `json:"method,omitempty" jsonschema:"description=Defaults to GET."`
	Headers    map[string]string `json:"headers,omitempty"`
	Body       string            `json:"body,omitempty" jsonschema:"description=Request body. JSON-encode objects yourself; Content-Type is not auto-set."`
	Timeout    int               `json:"timeout,omitempty"`
	SaveAs     string            `json:"saveAs,omitempty" jsonschema:"description=Storage path under a writable directory. When set, the response body is streamed to this path."`
	Raw        bool              `json:"raw,omitempty" jsonschema:"description=Skip HTML to markdown conversion (default is to convert HTML)."`
	AllHeaders bool              `json:"allHeaders,omitempty"`
}

type WebSearchInput struct {
	Query string `json:"query"`
	Count int    `json:"count,omitempty" jsonschema:"description=Default 5."`
}

type AnalyzeImageInput struct {
	Path     string `json:"path"`
	Question string `json:"question,omitempty"`
}

type TranscribeAudioInput struct {
	Path     string `json:"path"`
	Language string `json:"language,omitempty" jsonschema:"description=ISO-639 hint, optional."`
	Prompt   string `json:"prompt,omitempty" jsonschema:"description=Optional prior-context hint for the transcriber."`
}

type GenerateImageInput struct {
	Prompt      string `json:"prompt"`
	SaveAs      string `json:"saveAs,omitempty" jsonschema:"description=Storage path; defaults to an auto scratch path."`
	Size        string `json:"size,omitempty"`
	AspectRatio string `json:"aspectRatio,omitempty"`
	Seed        *int64 `json:"seed,omitempty"`
}

type SpeakInput struct {
	Text         string   `json:"text"`
	SaveAs       string   `json:"saveAs,omitempty"`
	Voice        string   `json:"voice,omitempty"`
	OutputFormat string   `json:"outputFormat,omitempty"`
	Speed        *float64 `json:"speed,omitempty"`
}

type EmbedInput struct {
	Text  string   `json:"text,omitempty" jsonschema:"description=A single text input. Mutually exclusive with 'texts'."`
	Texts []string `json:"texts,omitempty"`
}

type QueryDBInput struct {
	SQL    string `json:"sql"`
	Params []any  `json:"params,omitempty" jsonschema:"description=Positional parameters bound as $1, $2, ..."`
}

type RequestUpgradeInput struct {
	Description string `json:"description"`
}

type ConnectionRequestInput struct {
	Method  string            `json:"method" jsonschema:"description=HTTP method (GET, POST, ...)."`
	Path    string            `json:"path" jsonschema:"description=Path appended to the connection's base URL."`
	Body    any               `json:"body,omitempty" jsonschema:"description=Request body. For request_json, an object/array is JSON-encoded; for the raw request tool, pass a string."`
	Headers map[string]string `json:"headers,omitempty"`
}
