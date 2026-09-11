package agentsdk

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/airlockrun/goai/tool"
)

type greetIn struct {
	Name string `json:"name"`
}

type greetOut struct {
	Greeting string `json:"greeting"`
}

func greetTool(name, desc string, fn tool.TypedFunc[greetIn, greetOut]) tool.Tool {
	return tool.Typed[greetIn, greetOut](name).Description(desc).Execute(fn).Build()
}

func TestRegisterTool_WrapsExecuteErrorWithName(t *testing.T) {
	a, _ := testAgent(t)
	a.RegisterTool(greetTool("broken_tool", "always errors",
		func(ctx context.Context, in greetIn) (greetOut, error) {
			return greetOut{}, errors.New("unexpected end of JSON input")
		}), AccessUser)
	rt := a.tools["broken_tool"]
	_, err := rt.Execute(context.Background(), []byte(`{}`), tool.CallOptions{})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "broken_tool") {
		t.Errorf("error %q should include tool name", err.Error())
	}
	if !strings.Contains(err.Error(), "unexpected end of JSON input") {
		t.Errorf("error %q should preserve underlying message", err.Error())
	}
}

func TestRegisterTool_PreservesLLMHint(t *testing.T) {
	a, _ := testAgent(t)
	a.RegisterTool(greetTool("search", "Search the web.",
		func(ctx context.Context, in greetIn) (greetOut, error) { return greetOut{}, nil }),
		AccessUser, WithLLMHint("expensive; cache results before re-calling"))
	rt, ok := a.tools["search"]
	if !ok {
		t.Fatal("expected tool 'search' to be registered")
	}
	if rt.llmHint != "expensive; cache results before re-calling" {
		t.Errorf("registeredTool.llmHint = %q, want hint preserved", rt.llmHint)
	}
}
