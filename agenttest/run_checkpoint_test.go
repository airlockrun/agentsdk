package agenttest

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/airlockrun/agentsdk/agentruntime"
	"github.com/airlockrun/agentsdk/wire"
	"github.com/airlockrun/goai/stream"
	"github.com/airlockrun/sol/session"
)

func TestRunCheckpointStoreRevisionAndIsolation(t *testing.T) {
	s := &runCheckpointStore{}
	cp := &agentruntime.Checkpoint{Version: 1, Revision: 1, ContractHash: "hash", Phase: agentruntime.PhaseReady, Append: []session.Message{{Role: "user", Content: "input"}}}
	if err := s.SaveCheckpoint(t.Context(), cp); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveCheckpoint(t.Context(), cp); err != nil {
		t.Fatal(err)
	}
	cp.Append[0].Content = "mutated"
	if err := s.SaveCheckpoint(t.Context(), cp); err == nil {
		t.Fatal("accepted changed retry")
	}
	messages, loaded := s.snapshot()
	if len(messages) != 1 || messages[0].Content != "input" || loaded.Append != nil {
		t.Fatalf("messages=%+v checkpoint=%+v", messages, loaded)
	}
	messages[0].Content = "alias"
	loaded.ContractHash = "alias"
	messages, loaded = s.snapshot()
	if messages[0].Content != "input" || loaded.ContractHash != "hash" {
		t.Fatal("snapshot aliases store")
	}
	cp.Revision = 3
	if err := s.SaveCheckpoint(t.Context(), cp); err == nil {
		t.Fatal("accepted revision gap")
	}
	cp.Revision = 2
	if err := s.SaveCheckpoint(t.Context(), cp); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.SaveCheckpoint(ctx, cp); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := s.LoadCheckpoint(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestLeafRunControllerBudgets(t *testing.T) {
	s := &runCheckpointStore{}
	if err := s.SaveCheckpoint(t.Context(), &agentruntime.Checkpoint{Revision: 1, Append: []session.Message{{Tokens: session.Tokens{Input: 10, Output: 5}}}}); err != nil {
		t.Fatal(err)
	}
	c := &leafRunController{maxSteps: 1, maxTokens: 15}
	if err := c.chargeUsage(stream.UsageFrom(10, 5)); err == nil {
		t.Fatal("accepted exhausted token budget")
	}
	if err := c.BeforeModel(t.Context()); err == nil {
		t.Fatal("ignored tokens")
	}
	if err := c.Complete(t.Context(), wire.AgentReply{}); err == nil {
		t.Fatal("completed over budget")
	}
	if err := s.Compact(t.Context(), nil, 0); err != nil {
		t.Fatal(err)
	}
	if err := c.BeforeModel(t.Context()); err == nil {
		t.Fatal("compaction reset token budget")
	}
	c.maxTokens = 0
	if err := c.BeforeModel(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := c.BeforeModel(t.Context()); err == nil {
		t.Fatal("ignored steps")
	}
	if _, err := c.Get(t.Context(), []string{"child"}); !errors.Is(err, errRunChildren) {
		t.Fatal(err)
	}
}

func TestRunBudgetAccounting(t *testing.T) {
	for _, tc := range []struct {
		name    string
		limit   int64
		usage   stream.Usage
		wantErr bool
	}{
		{"missing input", 20, stream.Usage{OutputTokens: stream.OutputTokens{Total: stream.IntPtr(5)}}, true},
		{"missing output", 20, stream.Usage{InputTokens: stream.InputTokens{Total: stream.IntPtr(5)}}, true},
		{"zero reported", 20, stream.UsageFrom(0, 0), false},
		{"missing unlimited", 0, stream.Usage{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &leafRunController{maxTokens: tc.limit}
			if err := c.chargeUsage(tc.usage); (err != nil) != tc.wantErr {
				t.Fatalf("err=%v", err)
			}
		})
	}
	c := &leafRunController{}
	if err := c.chargeUsage(stream.UsageFrom(math.MaxInt, math.MaxInt)); err != nil {
		t.Fatal(err)
	}
	if err := c.chargeUsage(stream.UsageFrom(10, 5)); err != nil {
		t.Fatal(err)
	}
	if c.tokenCount() != math.MaxInt64 {
		t.Fatal("token accumulator overflowed")
	}
}
