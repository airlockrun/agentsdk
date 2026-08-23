package agentsdk

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/airlockrun/agentsdk/wire"
)

func TestNewRequiresDatabaseURL(t *testing.T) {
	t.Setenv("AIRLOCK_AGENT_MODE", "")
	t.Setenv("AIRLOCK_AGENT_ID", "test-agent")
	t.Setenv("AIRLOCK_API_URL", "http://127.0.0.1")
	t.Setenv("AIRLOCK_AGENT_TOKEN", "test-token")
	t.Setenv("AIRLOCK_DB_URL", "")

	defer func() {
		got := fmt.Sprint(recover())
		want := "agentsdk: required environment variable AIRLOCK_DB_URL is not set"
		if got != want {
			t.Fatalf("panic = %q, want %q", got, want)
		}
	}()
	New(Config{Description: "test"})
}

func TestNewRejectsUnknownAgentMode(t *testing.T) {
	t.Setenv("AIRLOCK_AGENT_MODE", "manifest")
	expectPanicContains(t, "unsupported AIRLOCK_AGENT_MODE: manifest", func() {
		New(Config{Description: "test"})
	})
}

func TestJobManifestModeServesCanonicalManifestWithoutRuntimeDependencies(t *testing.T) {
	t.Setenv("AIRLOCK_AGENT_MODE", "job-manifest")
	t.Setenv("AIRLOCK_AGENT_ID", "")
	t.Setenv("AIRLOCK_API_URL", "")
	t.Setenv("AIRLOCK_AGENT_TOKEN", "")
	t.Setenv("AIRLOCK_DB_URL", "postgres://must-not-be-opened")

	a := New(Config{Description: "manifest test"})
	if a.db != nil || a.client != nil || a.httpClient != nil {
		t.Fatalf("manifest mode initialized runtime dependencies: db=%p client=%p http=%p", a.db, a.client, a.httpClient)
	}
	second := RegisterJob(a, testJobDefinition(2))
	first := RegisterJob(a, testJobDefinition(1))
	second.Cron(&JobCron[testJobInput]{Slug: "z_hourly", Schedule: "@hourly", Description: "Hourly conversion."})
	first.Cron(&JobCron[testJobInput]{Slug: "a_daily", Schedule: "@daily", Description: "Daily conversion."})

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = writer
	defer func() {
		os.Stdout = stdout
		_ = writer.Close()
		_ = reader.Close()
	}()
	a.Serve()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	os.Stdout = stdout
	output, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if len(output) == 0 || output[len(output)-1] != '\n' || strings.Count(string(output), "\n") != 1 {
		t.Fatalf("manifest output is not exactly one JSON line: %q", output)
	}
	var manifest wire.JobManifest
	if err := json.Unmarshal(output, &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if len(manifest.JobHandlers) != 2 || manifest.JobHandlers[0].Version != 1 || manifest.JobHandlers[1].Version != 2 {
		t.Fatalf("job handlers are not canonical: %+v", manifest.JobHandlers)
	}
	if len(manifest.JobCrons) != 2 || manifest.JobCrons[0].Slug != "a_daily" || manifest.JobCrons[1].Slug != "z_hourly" {
		t.Fatalf("job crons are not canonical: %+v", manifest.JobCrons)
	}
	expected, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if string(output) != string(expected)+"\n" {
		t.Fatalf("manifest output is not canonical:\n%s", output)
	}
	expectPanicContains(t, "registrations are frozen", func() {
		RegisterJob(a, testJobDefinition(3))
	})
}

func TestJobManifestModeRuntimeMethodsFailLoudly(t *testing.T) {
	t.Setenv("AIRLOCK_AGENT_MODE", "job-manifest")
	a := New(Config{Description: "manifest test"})

	expectPanicContains(t, "DB is unavailable when AIRLOCK_AGENT_MODE=job-manifest", func() {
		a.DB()
	})
	if _, err := a.AgentURL(); err == nil || !strings.Contains(err.Error(), "AIRLOCK_AGENT_MODE=job-manifest") {
		t.Fatalf("AgentURL error = %v", err)
	}
	handle := RegisterJob(a, testJobDefinition(1))
	if _, err := handle.Get(t.Context(), testJobID); err == nil || !strings.Contains(err.Error(), "AIRLOCK_AGENT_MODE=job-manifest") {
		t.Fatalf("Get error = %v", err)
	}
}
