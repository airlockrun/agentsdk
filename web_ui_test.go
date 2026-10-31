package agentsdk

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestWebUI(t *testing.T) {
	for _, tt := range []struct {
		name    string
		options WebUIOptions
		config  string
	}{
		{"standard", WebUIOptions{}, "false"},
		{"navigation disabled", WebUIOptions{DisableNavigation: true}, "true"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var output bytes.Buffer
			if err := WebUI(tt.options).Render(context.Background(), &output); err != nil {
				t.Fatal(err)
			}
			want := `<link rel="stylesheet" href="/__air/assets/web-ui-v1.css"/><script src="/__air/assets/web-ui-v1.js" data-airlock-disable-navigation="` + tt.config + `"></script>`
			if output.String() != want {
				t.Fatalf("HTML = %q, want %q", output.String(), want)
			}
			if strings.Contains(output.String(), "onclick") || strings.Contains(output.String(), "<script>") {
				t.Fatal("component emits inline JavaScript")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := WebUI(WebUIOptions{}).Render(ctx, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled render = %v", err)
	}
}
