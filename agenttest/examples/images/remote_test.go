//go:build airlockintegration

package imagesexample

import (
	"context"
	"encoding/json"
	"github.com/airlockrun/agentsdk/remote"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/structpb"
	"testing"
	"time"
)

func TestRemoteStoredImage(t *testing.T) {
	client, err := remote.TestClientFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	input, _ := structpb.NewValue(map[string]any{})
	run, err := client.StartAgent(ctx, "image_probe", uuid.NewString(), "", input)
	if err != nil {
		t.Fatal(err)
	}
	run, err = client.WaitAgent(ctx, "image_probe", run.ID, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "completed" || run.Reply == nil || run.Reply.Kind != "output" {
		t.Fatalf("real image task = %+v", run)
	}
	var out Output
	if err := json.Unmarshal(run.Reply.Output, &out); err != nil || out.Description == "" {
		t.Fatalf("output=%s error=%v", run.Reply.Output, err)
	}
}
