package wire

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

const (
	MaxAuditActions      = 128
	MaxAuditActionBytes  = 8 << 10
	MaxAuditActionsBytes = 64 << 10
	MaxAuditLogs         = 256
	MaxAuditLogBytes     = 32 << 10
	MaxAuditErrorBytes   = 2 << 10
	MaxAuditPanicBytes   = 8 << 10
)

// BoundedAuditText bounds serialized JSON as well as UTF-8 text. JSON escaping
// can expand a byte sixfold; checking encoded length makes that bound explicit.
func BoundedAuditText(text string, limit int) string {
	if limit <= 0 {
		return ""
	}
	text = strings.ReplaceAll(strings.ToValidUTF8(text, "�"), "\x00", "�")
	if len(text) > limit {
		text = text[:limit]
	}
	for len(text) > 0 {
		for !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
		encoded, _ := json.Marshal(text)
		if len(encoded) <= limit {
			return text
		}
		text = text[:len(text)/2]
	}
	return ""
}

func BoundAuditActions(actions []Action) []Action {
	out := make([]Action, 0, min(len(actions), MaxAuditActions))
	used := 2
	for _, action := range actions {
		if len(out) == MaxAuditActions {
			break
		}
		action.Type = BoundedAuditText(action.Type, 128)
		action.Error = BoundedAuditText(action.Error, MaxAuditErrorBytes)
		raw, err := json.Marshal(action)
		if err != nil || len(raw) > MaxAuditActionBytes {
			action.Request, action.Response = nil, nil
			raw, err = json.Marshal(action)
		}
		if err != nil {
			continue
		}
		// Normalize arbitrary selected values through JSON with exact numbers.
		// PostgreSQL text/JSONB rejects NUL and invalid UTF-8; optional telemetry
		// must not prevent an otherwise valid execution from settling.
		var normalized Action
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if err := decoder.Decode(&normalized); err != nil {
			continue
		}
		normalized.Request = auditJSONValue(normalized.Request)
		normalized.Response = auditJSONValue(normalized.Response)
		action = normalized
		raw, err = json.Marshal(action)
		if err != nil || len(raw) > MaxAuditActionBytes {
			continue
		}
		if used+len(raw)+1 > MaxAuditActionsBytes {
			break
		}
		out = append(out, action)
		used += len(raw) + 1
	}
	return out
}

func auditJSONValue(value any) any {
	switch v := value.(type) {
	case string:
		return strings.ReplaceAll(strings.ToValidUTF8(v, "�"), "\x00", "�")
	case []any:
		for i := range v {
			v[i] = auditJSONValue(v[i])
		}
		return v
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[strings.ReplaceAll(strings.ToValidUTF8(key, "�"), "\x00", "�")] = auditJSONValue(item)
		}
		return out
	default:
		return value
	}
}

func BoundAuditLogs(logs []LogEntry) []LogEntry {
	out := make([]LogEntry, 0, min(len(logs), MaxAuditLogs))
	used := 2
	for _, entry := range logs {
		if len(out) == MaxAuditLogs {
			break
		}
		entry.Message = BoundedAuditText(entry.Message, min(4096, MaxAuditLogBytes-used-128))
		switch entry.Level {
		case LogLevelDebug, LogLevelInfo, LogLevelWarn, LogLevelError:
		default:
			entry.Level = LogLevelInfo
		}
		raw, _ := json.Marshal(entry)
		if used+len(raw)+1 > MaxAuditLogBytes {
			break
		}
		out = append(out, entry)
		used += len(raw) + 1
		if used+128 >= MaxAuditLogBytes {
			break
		}
	}
	return out
}
