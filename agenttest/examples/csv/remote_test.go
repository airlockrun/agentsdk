//go:build airlockintegration

package csvexample

import (
	"context"
	"encoding/json"
	"github.com/airlockrun/agentsdk/remote"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/structpb"
	"testing"
	"time"
)

// The candidate runner supplies the exact real-host capability and revision.
// Missing configuration fails; this suite never chooses a local/model fallback.
func TestRemoteConversion(t *testing.T) {
	client, err := remote.TestClientFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	input, _ := structpb.NewValue(map[string]any{"header": []any{"name", "note", "quantity"}, "rows": []any{[]any{"Alice", `comma, quote "ok"`, "2"}, []any{"Bob", "two\nlines", "3"}, []any{"Саша", "Unicode", "4"}}})
	run, err := client.StartAgent(ctx, "convert_rows", uuid.NewString(), "", input)
	if err != nil {
		t.Fatal(err)
	}
	run, err = client.WaitAgent(ctx, "convert_rows", run.ID, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "completed" || run.Reply == nil || run.Reply.Kind != "output" {
		t.Fatalf("real task = %+v", run)
	}
	var out Output
	if err := json.Unmarshal(run.Reply.Output, &out); err != nil || out.Filename != Filename || out.Rows != 3 {
		t.Fatalf("reply=%s error=%v", run.Reply.Output, err)
	}
}
