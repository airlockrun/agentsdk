package prompt

import (
	"strings"
	"testing"
)

// extractEnv pulls the <env>…</env> block out of a rendered prompt.
func extractEnv(t *testing.T, s string) string {
	t.Helper()
	i := strings.Index(s, "<env>")
	j := strings.Index(s, "</env>")
	if i < 0 || j < 0 {
		t.Fatalf("no <env> block in:\n%s", s)
	}
	return s[i : j+len("</env>")]
}

func TestEnvBlockRender(t *testing.T) {
	tests := []struct {
		name string
		data AgentData
		want string
	}{
		{
			name: "telegram with user + email shows rendering note",
			data: AgentData{Date: "2026-06-04", Platform: "telegram", UserName: "Jane Doe", UserEmail: "jane@example.com", Conversation: "7f3a"},
			want: "<env>\nDate: 2026-06-04\nPlatform: telegram\nUser: Jane Doe <jane@example.com>\nConversation: 7f3a\nRendering: this channel doesn't render Markdown tables or headings — use short lines, bullet lists, or \"key: value\" pairs instead of tables.\n</env>",
		},
		{
			name: "web omits rendering note",
			data: AgentData{Date: "2026-06-04", Platform: "web", UserName: "Jane Doe", Conversation: "c1"},
			want: "<env>\nDate: 2026-06-04\nPlatform: web\nUser: Jane Doe\nConversation: c1\n</env>",
		},
		{
			name: "a2a no user, no note",
			data: AgentData{Date: "2026-06-04", Platform: "a2a", Conversation: "c2"},
			want: "<env>\nDate: 2026-06-04\nPlatform: a2a\nConversation: c2\n</env>",
		},
		{
			name: "empty platform omits the line (no inference)",
			data: AgentData{Date: "2026-06-04"},
			want: "<env>\nDate: 2026-06-04\n</env>",
		},
		{
			name: "discord shows rendering note",
			data: AgentData{Date: "2026-06-04", Platform: "discord"},
			want: "<env>\nDate: 2026-06-04\nPlatform: discord\nRendering: this channel doesn't render Markdown tables or headings — use short lines, bullet lists, or \"key: value\" pairs instead of tables.\n</env>",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := Render(tt.data, "user")
			if err != nil {
				t.Fatalf("Render: %v", err)
			}
			if got := extractEnv(t, out); got != tt.want {
				t.Fatalf("env block mismatch:\n got: %q\nwant: %q", got, tt.want)
			}
		})
	}
}
