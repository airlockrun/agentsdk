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
	a.RegisterTool(&Tool[greetIn, greetOut]{
		Name:        "pub_hello",
		Description: "Public hello.",
		Access:      AccessPublic,
		Execute:     func(ctx context.Context, in greetIn) (greetOut, error) { return greetOut{}, nil },
	})
	a.RegisterTool(&Tool[greetIn, greetOut]{
		Name:        "user_hello",
		Description: "User hello.",
		Access:      AccessUser,
		Execute:     func(ctx context.Context, in greetIn) (greetOut, error) { return greetOut{}, nil },
	})
	a.RegisterTool(&Tool[greetIn, greetOut]{
		Name:        "admin_hello",
		Description: "Admin hello.",
		Access:      AccessAdmin,
		Execute:     func(ctx context.Context, in greetIn) (greetOut, error) { return greetOut{}, nil },
	})

	cases := []struct {
		access      Access
		mustHave    []string
		mustNotHave []string
	}{
		{AccessPublic, []string{"pub_hello"}, []string{"user_hello", "admin_hello", "run_js", "httpRequest", "queryDB"}},
		{AccessUser, []string{"pub_hello", "user_hello", "httpRequest"}, []string{"admin_hello", "run_js", "queryDB"}},
		{AccessAdmin, []string{"pub_hello", "user_hello", "admin_hello", "httpRequest", "queryDB"}, []string{"run_js"}},
	}
	for _, c := range cases {
		t.Run(string(c.access), func(t *testing.T) {
			run := newRun(a, "rd-"+string(c.access), "", "", context.Background())
			run.directTools = true
			run.callerAccess = c.access
			ts := buildSolTools(a, run, nil)
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
	ts := buildSolTools(a, run, nil)
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
	ts := buildSolTools(a, run, nil)
	if _, ok := ts["run_js"]; !ok {
		t.Fatalf("JS path must expose run_js; got tools: %v", keys(ts))
	}
	if _, ok := ts["fileRead"]; ok {
		t.Fatalf("JS path must NOT expose fileRead as a top-level tool; got tools: %v", keys(ts))
	}
}

// TestDirectTools_RegisteredToolExecutes proves the wrapped Execute
// closure actually unmarshals input, calls the user fn, and returns
// the JSON-marshaled output.
func TestDirectTools_RegisteredToolExecutes(t *testing.T) {
	a, _ := testAgent(t)
	a.RegisterTool(&Tool[greetIn, greetOut]{
		Name:        "echo_name",
		Description: "Echo the name back.",
		Access:      AccessPublic,
		Execute: func(ctx context.Context, in greetIn) (greetOut, error) {
			return greetOut{Greeting: "hi " + in.Name}, nil
		},
	})
	run := newRun(a, "rd-exec", "", "", context.Background())
	run.directTools = true
	run.callerAccess = AccessPublic

	ts := buildSolTools(a, run, nil)
	t1, ok := ts["echo_name"]
	if !ok {
		t.Fatalf("echo_name should be exposed; got %v", keys(ts))
	}
	res, err := t1.Execute(context.Background(), json.RawMessage(`{"name":"Sol"}`), tool.CallOptions{})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(res.Output, "hi Sol") {
		t.Fatalf("expected greeting in output, got %q", res.Output)
	}
}

// TestDirectTools_BuiltinShadowsRegistered confirms that a registered
// tool with a name that collides with a built-in (e.g. `fileRead`) is
// silently overwritten by the built-in in direct mode — matching what
// the JS VM does today via vm.Set last-write. The author's intent is
// preserved at registration (no panic), but at the LLM surface the
// built-in wins. Symmetric behaviour across both modes.
func TestDirectTools_BuiltinShadowsRegistered(t *testing.T) {
	a, _ := testAgent(t)
	a.RegisterTool(&Tool[greetIn, greetOut]{
		Name:        "fileRead",
		Description: "Author-shadowed fileRead.",
		Access:      AccessUser,
		Execute: func(ctx context.Context, in greetIn) (greetOut, error) {
			return greetOut{Greeting: "AUTHOR"}, nil
		},
	})
	run := newRun(a, "rd-shadow", "", "", context.Background())
	run.directTools = true
	run.callerAccess = AccessUser
	ts := buildSolTools(a, run, nil)
	got, ok := ts["fileRead"]
	if !ok {
		t.Fatalf("fileRead should be present in tool set")
	}
	if strings.Contains(got.Description, "Author-shadowed") {
		t.Errorf("expected built-in fileRead to shadow the registered tool; got author description %q", got.Description)
	}
	if !strings.Contains(strings.ToLower(got.Description), "stored file") {
		t.Errorf("expected built-in description to win; got %q", got.Description)
	}
}

func keys(ts tool.Set) []string {
	out := make([]string, 0, len(ts))
	for k := range ts {
		out = append(out, k)
	}
	return out
}
