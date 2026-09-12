package agentsdk

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/airlockrun/agentsdk/wire"
)

func TestBorrowedInvocationCallbackHeaders(t *testing.T) {
	a := &Agent{}
	scope := wire.RuntimeContext{RunID: "run", InvocationToken: strings.Repeat("12", 32), Caller: testWireCaller("user", wire.AccessUser),
		Job: &wire.RuntimeJobContext{ID: "job", Attempt: 3, LeaseToken: "lease"}}
	run := a.borrowRuntimeRun(context.Background(), scope)
	req, err := newAirlockClient("https://airlock.test", "app-token", http.DefaultClient).newRequest(run.ctx, http.MethodPost, "/api/agent/jobs", nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		wire.InvocationTokenHeader: scope.InvocationToken,
		"X-Airlock-Run-ID":         scope.RunID,
		"X-Airlock-Job-ID":         scope.Job.ID,
		"X-Airlock-Job-Attempt":    "3",
		jobLeaseTokenHeader:        scope.Job.LeaseToken,
	} {
		if got := req.Header.Get(name); got != want {
			t.Errorf("%s=%q want %q", name, got, want)
		}
		if got := run.callbackHeaders()[name]; got != want {
			t.Errorf("model header %s=%q want %q", name, got, want)
		}
	}
}

func TestRuntimeContextRequiresInvocationProof(t *testing.T) {
	a := &Agent{agentID: "app"}
	valid := runtimeRequest(a, "tool")
	for _, token := range []string{"", valid.Context.RunID, strings.Repeat("z", 64), strings.Repeat("12", 31)} {
		t.Run(token, func(t *testing.T) {
			supplied := valid.Context
			supplied.InvocationToken = token
			if err := validateRuntimeContext(supplied); err == nil {
				t.Fatal("missing or malformed receipt accepted")
			}
		})
	}
	if err := validateRuntimeContext(valid.Context); err != nil {
		t.Fatal(err)
	}
}
