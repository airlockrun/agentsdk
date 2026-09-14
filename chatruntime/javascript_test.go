package chatruntime_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/airlockrun/agentsdk/capability"
	"github.com/airlockrun/agentsdk/chatruntime"
	"github.com/airlockrun/agentsdk/jsexec"
	"github.com/airlockrun/goai/tool"
)

func TestJavaScriptToolApplicationLifecycle(t *testing.T) {
	for _, execute := range []bool{false, true} {
		t.Run(map[bool]string{false: "lazy", true: "execute"}[execute], func(t *testing.T) {
			e, allocations := &executor{}, 0
			js, close, err := chatruntime.JavaScriptTool(t.Context(), nil, backendFunc(func(context.Context, chatruntime.Invocation) (tool.Result, error) {
				t.Fatal("unexpected callback")
				return tool.Result{}, nil
			}), func(context.Context, []capability.Definition) (jsexec.Session, error) { allocations++; return e, nil })
			if err != nil {
				t.Fatal(err)
			}
			if execute {
				if _, err := js.Execute(t.Context(), json.RawMessage(`{"code":"return 42;","description":"calculate","request_confirmation":true}`), tool.CallOptions{ToolCallID: "task"}); err != nil {
					t.Fatal(err)
				}
			}
			if err := close(); err != nil {
				t.Fatal(err)
			}
			want := 0
			if execute {
				want = 1
			}
			if allocations != want || e.calls != want || e.closes != want {
				t.Fatalf("allocations=%d executor=%+v", allocations, e)
			}
		})
	}
}
