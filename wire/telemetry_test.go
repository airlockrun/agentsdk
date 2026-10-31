package wire

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestAuditBoundsApplyToEveryPayloadKey(t *testing.T) {
	for _, value := range []string{strings.Repeat("x", 1<<20), strings.Repeat("\x00", 1<<20), strings.Repeat("世界", 1<<18)} {
		actions := make([]Action, 1000)
		logs := make([]LogEntry, 1000)
		for i := range actions {
			actions[i] = Action{Type: value, Request: map[string]any{"arbitrary": value}, Response: map[string]any{"nested": []any{value}}, Error: value}
			logs[i] = LogEntry{Message: value, Level: LogLevel(value)}
		}
		bounded := BoundAuditActions(actions)
		raw, err := json.Marshal(bounded)
		if err != nil || len(raw) > MaxAuditActionsBytes || len(bounded) > MaxAuditActions {
			t.Fatalf("actions count=%d bytes=%d %v", len(bounded), len(raw), err)
		}
		boundedLogs := BoundAuditLogs(logs)
		raw, err = json.Marshal(boundedLogs)
		if err != nil || len(raw) > MaxAuditLogBytes || len(boundedLogs) > MaxAuditLogs {
			t.Fatalf("logs count=%d bytes=%d %v", len(boundedLogs), len(raw), err)
		}
		for _, limit := range []int{0, MaxAuditErrorBytes, MaxAuditPanicBytes} {
			raw, _ := json.Marshal(BoundedAuditText(value, limit))
			if len(raw) > max(2, limit) {
				t.Fatalf("text bytes=%d limit=%d", len(raw), limit)
			}
		}
	}
}

func TestAuditTextAndJSONArePostgresSafe(t *testing.T) {
	text := BoundedAuditText("a\x00\xff世界", 8)
	if strings.ContainsRune(text, 0) || !utf8.ValidString(text) {
		t.Fatalf("unsafe audit text %q", text)
	}
	input := []Action{{Type: "test", Request: map[string]any{"value": "a\x00b", "literal": `\u0000`, "number": json.Number("9007199254740993")}}}
	bounded := BoundAuditActions(input)
	raw, err := json.Marshal(bounded)
	if err != nil || bytesContainNUL(raw) || !strings.Contains(string(raw), "9007199254740993") {
		t.Fatalf("unsafe or lossy action: %s %v", raw, err)
	}
	values := bounded[0].Request.(map[string]any)
	if values["literal"] != `\u0000` {
		t.Fatal("changed a literal escape sequence")
	}
}

func bytesContainNUL(raw []byte) bool {
	var actions []Action
	if json.Unmarshal(raw, &actions) != nil {
		return true
	}
	for _, action := range actions {
		values := action.Request.(map[string]any)
		if strings.ContainsRune(values["value"].(string), 0) {
			return true
		}
	}
	return false
}
