package agentsdk

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/airlockrun/agentsdk/wire"
)

func TestEnvVarHandleStartupSnapshot(t *testing.T) {
	for _, tt := range []struct {
		name                    string
		values                  map[string]string
		fallback, pattern, want string
		wantError               bool
	}{
		{name: "configured", values: map[string]string{"region": "eu"}, fallback: "us", want: "eu"},
		{name: "default", values: map[string]string{}, fallback: "us", want: "us"},
		{name: "explicit empty", values: map[string]string{"region": ""}, fallback: "us", want: ""},
		{name: "optional unset", values: map[string]string{}, want: ""},
		{name: "required unset", values: map[string]string{}, pattern: "^.+$", wantError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a := &Agent{phase: agentRunning, envConfig: wire.EnvConfigSnapshot{Protocol: wire.EnvConfigProtocol, Values: tt.values}}
			h := &EnvVarHandle{slug: "region", defaultValue: tt.fallback, agent: a}
			if tt.pattern != "" {
				h.pattern = regexp.MustCompile(tt.pattern)
			}
			got, err := h.Get(context.Background())
			if (err != nil) != tt.wantError || got != tt.want {
				t.Fatalf("Get = %q, %v", got, err)
			}
		})
	}
}

func TestEnvConfigurationImmutableAndSecretRedacted(t *testing.T) {
	const secret = "snapshot-secret-value"
	raw, _ := json.Marshal(wire.EnvConfigSnapshot{Protocol: wire.EnvConfigProtocol, Revision: 4, Attempt: "start-attempt", Values: map[string]string{"key": secret}})
	t.Setenv(wire.EnvConfigVariable, string(raw))
	a := &Agent{phase: agentRunning, sensitiveSet: make(map[string]struct{}), envVars: map[string]*EnvVar{"key": {Secret: true}}}
	if err := a.loadEnvConfig(); err != nil {
		t.Fatal(err)
	}
	if _, present := os.LookupEnv(wire.EnvConfigVariable); present {
		t.Fatal("private configuration payload can leak into child process environments")
	}
	t.Setenv(wire.EnvConfigVariable, `{"protocol":"changed","values":{"key":"changed"}}`)
	h := &EnvVarHandle{slug: "key", secret: true, agent: a}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if got, err := h.Get(context.Background()); got != secret || err != nil {
				t.Errorf("Get did not retain startup snapshot")
			}
		})
	}
	wg.Wait()
	if _, ok := a.sensitiveSet[secret]; !ok {
		t.Fatal("startup secret is not in the redact set")
	}
}

func TestEnvConfigurationFailsWithoutCompatibleHost(t *testing.T) {
	for _, raw := range []string{"", "not-json", `{"protocol":"unknown","revision":1,"attempt":"x","values":{"key":"do-not-leak"}}`} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv(wire.EnvConfigVariable, raw)
			a := &Agent{envVars: map[string]*EnvVar{"key": {Secret: true}}}
			err := a.loadEnvConfig()
			if err == nil || strings.Contains(err.Error(), "do-not-leak") {
				t.Fatalf("invalid configuration error: %v", err)
			}
		})
	}
}
