package agentsdk

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/airlockrun/goai/tool"
)

// TestDirectTools_RegisteredToolSurface_AccessGated verifies that the
// direct-tools surface filters RegisteredTools by Access in the same
// way newVM filters JS bindings. A public-tier run must see only
// public tools; a user-tier run must see public+user; an admin-tier
// run sees everything.
func TestDirectTools_RegisteredToolSurface_AccessGated(t *testing.T) {
	a, _ := testAgent(t)
	noop := func(ctx context.Context, in greetIn) (greetOut, error) { return greetOut{}, nil }
	a.RegisterTool(greetTool("pub_hello", "Public hello.", noop), AccessPublic)
	a.RegisterTool(greetTool("user_hello", "User hello.", noop), AccessUser)
	a.RegisterTool(greetTool("admin_hello", "Admin hello.", noop), AccessAdmin)

	cases := []struct {
		access      Access
		mustHave    []string
		mustNotHave []string
	}{
		{AccessPublic, []string{"tool__pub_hello"}, []string{"tool__user_hello", "tool__admin_hello", "run_js", "air__http_request", "air__query_db"}},
		{AccessUser, []string{"tool__pub_hello", "tool__user_hello", "air__http_request"}, []string{"tool__admin_hello", "run_js", "air__query_db"}},
		{AccessAdmin, []string{"tool__pub_hello", "tool__user_hello", "tool__admin_hello", "air__http_request", "air__query_db"}, []string{"run_js"}},
	}
	for _, c := range cases {
		t.Run(string(c.access), func(t *testing.T) {
			run := newRun(a, "rd-"+string(c.access), "", "", context.Background())
			run.directTools = true
			run.callerAccess = c.access
			ts := buildSolTools(a, run)
			for _, name := range c.mustHave {
				if _, ok := ts[name]; !ok {
					t.Errorf("%s tier: missing tool %q (have %v)", c.access, name, keys(ts))
				}
			}
			for _, name := range c.mustNotHave {
				if _, ok := ts[name]; ok {
					t.Errorf("%s tier: must not expose %q", c.access, name)
				}
			}
		})
	}
}

// TestDirectTools_RunJSAbsent confirms that direct mode replaces (not
// supplements) the run_js surface — `run_js` is not in the tool set.
func TestDirectTools_RunJSAbsent(t *testing.T) {
	a, _ := testAgent(t)
	run := newRun(a, "rd-norjs", "", "", context.Background())
	run.directTools = true
	run.callerAccess = AccessPublic
	ts := buildSolTools(a, run)
	if _, ok := ts["run_js"]; ok {
		t.Fatalf("direct mode must not expose run_js; got tools: %v", keys(ts))
	}
}

// TestDirectTools_JSPathUnchanged verifies the legacy JS path still
// exposes run_js (and only run_js + maybe promptAgent) when DirectTools
// is unset — regression guard for the buildSolTools branch.
func TestDirectTools_JSPathUnchanged(t *testing.T) {
	a, _ := testAgent(t)
	run := newRun(a, "rd-js", "", "", context.Background())
	run.callerAccess = AccessUser
	ts := buildSolTools(a, run)
	if _, ok := ts["run_js"]; !ok {
		t.Fatalf("JS path must expose run_js; got tools: %v", keys(ts))
	}
	if _, ok := ts["air__file_read"]; ok {
		t.Fatalf("JS path must NOT expose air__file_read as a top-level tool; got tools: %v", keys(ts))
	}
}

// TestDirectTools_RegisteredToolExecutes proves the wrapped Execute
// closure actually unmarshals input, calls the user fn, and returns
// the JSON-marshaled output.
func TestDirectTools_RegisteredToolExecutes(t *testing.T) {
	a, _ := testAgent(t)
	a.RegisterTool(greetTool("echo_name", "Echo the name back.",
		func(ctx context.Context, in greetIn) (greetOut, error) {
			return greetOut{Greeting: "hi " + in.Name}, nil
		}), AccessPublic)
	run := newRun(a, "rd-exec", "", "", context.Background())
	run.directTools = true
	run.callerAccess = AccessPublic

	ts := buildSolTools(a, run)
	t1, ok := ts["tool__echo_name"]
	if !ok {
		t.Fatalf("tool__echo_name should be exposed; got %v", keys(ts))
	}
	res, err := t1.Execute(context.Background(), json.RawMessage(`{"name":"Sol"}`), tool.CallOptions{})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(res.Output, "hi Sol") {
		t.Fatalf("expected greeting in output, got %q", res.Output)
	}
}

func TestDirectTools_RegisteredAndBuiltinNamespacesDoNotCollide(t *testing.T) {
	a, _ := testAgent(t)
	a.RegisterTool(greetTool("output", "Author output tool.",
		func(ctx context.Context, in greetIn) (greetOut, error) {
			return greetOut{Greeting: "AUTHOR"}, nil
		}), AccessUser)
	run := newRun(a, "rd-shadow", "", "", context.Background())
	run.directTools = true
	run.callerAccess = AccessUser
	ts := buildSolTools(a, run)
	author, ok := ts["tool__output"]
	if !ok {
		t.Fatal("tool__output should be present")
	}
	if !strings.Contains(author.Description, "Author output") {
		t.Fatalf("registered description = %q", author.Description)
	}
	builtin, ok := ts["air__output"]
	if !ok {
		t.Fatal("air__output should be present")
	}
	if strings.Contains(builtin.Description, "Author output") {
		t.Fatalf("builtin description = %q", builtin.Description)
	}
}

func keys(ts tool.Set) []string {
	out := make([]string, 0, len(ts))
	for k := range ts {
		out = append(out, k)
	}
	return out
}
