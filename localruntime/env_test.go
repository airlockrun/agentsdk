package localruntime

import (
	"github.com/airlockrun/agentsdk/wire"
	"net/http/httptest"
	"testing"
)

func TestDeclaredEnvironmentBindingsAreExplicit(t *testing.T) {
	h, state, _, _ := testHost(t, completeModel(t))
	config := h.config
	config.EnvVars = map[string]string{"region": "fixture-region"}
	host, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.update(t.Context(), func(d *stateData) error {
		d.Manifest.EnvVars = []wire.EnvVarDef{{Slug: "region"}, {Slug: "required"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(host.Handler())
	defer server.Close()
	status, body := hostRequest(t, server, "GET", "/api/agent/env-vars/region", nil, nil)
	if status != 200 || string(body) != "{\"value\":\"fixture-region\"}\n" {
		t.Fatalf("binding %d %s", status, body)
	}
	for _, slug := range []string{"required", "undeclared"} {
		if status, _ := hostRequest(t, server, "GET", "/api/agent/env-vars/"+slug, nil, nil); status != 404 {
			t.Fatal("missing binding returned successful data")
		}
	}
}
