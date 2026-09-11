package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	airlockv1 "github.com/airlockrun/agentsdk/internal/airlockv1"
)

const testBuildID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
const testBuildAgentID = "11111111-1111-1111-1111-111111111111"

func TestParseDeployBuildFlags(t *testing.T) {
	for _, tt := range []struct {
		name, command string
		args          []string
		wantErr       string
	}{
		{"defaults", "list", nil, ""},
		{"interspersed", "list", []string{"--limit", "50", "repo", "--json", "--agent", "todo", "--remote", "prod", "--url", "https://example.com"}, ""},
		{"status", "status", []string{"repo", "--build", testBuildID, "--logs", "--watch"}, ""},
		{"json logs", "status", []string{"--json", "--logs"}, ""},
		{"zero", "list", []string{"--limit", "0"}, "between 1 and 50"},
		{"negative", "list", []string{"--limit", "-1"}, "between 1 and 50"},
		{"large", "list", []string{"--limit", "51"}, "between 1 and 50"},
		{"not number", "list", []string{"--limit", "ten"}, "between 1 and 50"},
		{"watch json", "status", []string{"--watch", "--json"}, "cannot combine"},
		{"list watch", "list", []string{"--watch"}, "requires deploy status"},
		{"list logs", "list", []string{"--logs"}, "requires deploy status"},
		{"list build", "list", []string{"--build", testBuildID}, "requires deploy status"},
		{"status limit", "status", []string{"--limit", "2"}, "requires deploy list"},
		{"bad build", "status", []string{"--build", "../bad"}, "UUID"},
		{"missing value", "status", []string{"--build"}, "needs a value"},
		{"flag as value", "status", []string{"--agent", "--json"}, "needs a value"},
		{"empty value", "list", []string{"--url", ""}, "needs a value"},
		{"bad remote", "list", []string{"--remote", "../bad"}, "invalid remote"},
		{"directories", "list", []string{"a", "b"}, "at most one"},
		{"unknown", "status", []string{"--force"}, "unknown"},
		{"end options", "list", []string{"--", "-repo"}, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f, err := parseDeployBuildFlags(tt.command, tt.args)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tt.name == "defaults" && (f.dir != "." || f.limit != 10) {
				t.Fatalf("defaults: %+v", f)
			}
			if tt.name == "interspersed" && (f.limit != 50 || f.dir != "repo" || !f.json || f.agent != "todo" || f.remote != "prod") {
				t.Fatalf("flags: %+v", f)
			}
		})
	}
}

func TestDeployBuildCommands(t *testing.T) {
	for _, tt := range []struct {
		name, command, status  string
		flags                  []string
		wantErr                string
		listCount, detailCount int
	}{
		{"list default", "list", "complete", nil, "", 1, 0},
		{"list limit json", "list", "failed", []string{"--limit", "1", "--json"}, "", 1, 0},
		{"latest", "status", "building", nil, "", 1, 1},
		{"explicit", "status", "complete", []string{"--build", testBuildID, "--logs"}, "", 0, 1},
		{"failed json", "status", "failed", []string{"--json"}, "failed", 1, 1},
		{"failed text", "status", "failed", []string{"--logs"}, "failed", 1, 1},
		{"empty list", "list", "empty", nil, "no builds", 1, 0},
		{"empty latest", "status", "empty", nil, "no builds", 1, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("AIRLOCK_INTEGRATION_TOKEN", "")
			var paths []string
			prefix := "/api/v1/agents/" + testBuildAgentID
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.RequestURI())
				if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer token" {
					t.Errorf("request: %s %s auth=%s", r.Method, r.URL, r.Header.Get("Authorization"))
				}
				switch r.URL.Path {
				case prefix:
					fmt.Fprintf(w, `{"agent":{"id":%q,"slug":"todo","status":"stopped"}}`, testBuildAgentID)
				case prefix + "/builds":
					resp := &airlockv1.ListAgentBuildsResponse{}
					if tt.status != "empty" {
						for i := 0; i < 12; i++ {
							resp.Builds = append(resp.Builds, &airlockv1.AgentBuildInfo{Id: testBuildID, AgentId: testBuildAgentID, Status: tt.status, Type: "upgrade", Instructions: "Fix retries"})
						}
					}
					body, err := protoMarshal.Marshal(resp)
					if err != nil {
						t.Error(err)
					}
					_, _ = w.Write(body)
				case prefix + "/builds/" + testBuildID:
					fmt.Fprintf(w, `{"build":{"id":%q,"agentId":%q,"status":%q,"type":"upgrade","sourceRef":"abc123","startedAt":"2026-09-09T12:00:00Z","deploymentPhase":"AGENT_BUILD_DEPLOYMENT_PHASE_PAUSED","deploymentPausedAt":"2026-09-09T12:01:00Z","deploymentDrainDeadline":"2026-09-09T12:03:00Z","errorMessage":"compile failed","exitStatus":"error","exitMessage":"Sol reason","dockerLog":"docker output\n","solLog":"sol output\n","jobBlockers":[{"handlerName":"resize","handlerVersion":1,"queuedCount":"2","runningCount":"1"}]}}`, testBuildID, testBuildAgentID, tt.status)
				default:
					t.Errorf("unexpected path %s", r.URL)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			if err := saveLoginCredentials(srv.URL, "test@example.com", "token", ""); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			binding := agentBinding{}
			binding.putRemote("prod", agentRemoteBinding{AirlockURL: srv.URL, AgentID: testBuildAgentID, Slug: "old-slug", SourceState: "unchanged"})
			if err := writeAgentBinding(dir, binding); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(filepath.Join(dir, agentBindingPath))
			if err != nil {
				t.Fatal(err)
			}
			args := append([]string{tt.command, dir, "--remote", "prod"}, tt.flags...)
			output, err := captureCommandStdoutResult(t, func() error { return cmdDeploy(args) })
			if tt.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error=%v", err)
			}
			wantPaths := []string{prefix}
			if tt.listCount > 0 {
				wantPaths = append(wantPaths, prefix+"/builds")
			}
			if tt.detailCount > 0 {
				wantPaths = append(wantPaths, prefix+"/builds/"+testBuildID)
			}
			if !reflect.DeepEqual(paths, wantPaths) {
				t.Fatalf("paths=%v want=%v", paths, wantPaths)
			}
			if strings.Contains(tt.name, "json") {
				if !json.Valid([]byte(output)) {
					t.Fatalf("invalid JSON: %s", output)
				}
				if tt.command == "list" {
					var resp airlockv1.ListAgentBuildsResponse
					if err := protoUnmarshal.Unmarshal([]byte(output), &resp); err != nil || len(resp.Builds) != 1 {
						t.Fatalf("list JSON: %s %v", output, err)
					}
				}
			} else if tt.status != "empty" {
				if !strings.Contains(output, testBuildID) {
					t.Fatalf("missing full ID: %s", output)
				}
				if tt.command == "status" {
					for _, want := range []string{"Build status: " + tt.status, "Source ref: abc123", "Job blocker: resize v1 queued=2 running=1", "Drain deadline:", "Error: compile failed", "Sol exit: error Sol reason"} {
						if !strings.Contains(output, want) {
							t.Errorf("output missing %q: %s", want, output)
						}
					}
					if strings.Contains(output, "currently deployed") || strings.Contains(output, "running agent") {
						t.Errorf("conflated runtime and build: %s", output)
					}
				}
				if tt.name == "list default" && strings.Count(output, testBuildID) != 10 {
					t.Fatalf("default limit: %s", output)
				}
				if tt.name == "explicit" && (!strings.Contains(output, "docker output") || !strings.Contains(output, "sol output")) {
					t.Fatalf("missing logs: %s", output)
				}
			}
			after, err := os.ReadFile(filepath.Join(dir, agentBindingPath))
			if err != nil || string(after) != string(before) {
				t.Fatalf("binding changed: %v", err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 || entries[0].Name() != ".airlock" {
				t.Fatalf("workspace changed: %v %v", entries, err)
			}
		})
	}
}

func TestDeployBuildWatch(t *testing.T) {
	for _, terminal := range []string{"complete", "failed", "cancel"} {
		t.Run(terminal, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("AIRLOCK_INTEGRATION_TOKEN", "")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			lists, details := 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer token" || r.Method != "GET" {
					t.Errorf("bad request %s", r.URL)
				}
				switch r.URL.Path {
				case "/api/v1/agents":
					fmt.Fprintf(w, `{"agents":[{"id":%q,"slug":"todo"}]}`, testBuildAgentID)
				case "/api/v1/agents/" + testBuildAgentID + "/builds":
					lists++
					fmt.Fprintf(w, `{"builds":[{"id":%q}]}`, testBuildID)
				case "/api/v1/agents/" + testBuildAgentID + "/builds/" + testBuildID:
					details++
					status, log := "building", "first\n"
					if details == 3 {
						log += "second\n"
					}
					if details >= 4 {
						status, log = terminal, "replacement\n"
					}
					if terminal == "cancel" {
						status = "building"
						cancel()
					}
					fmt.Fprintf(w, `{"build":{"id":%q,"agentId":%q,"status":%q,"dockerLog":%q,"solLog":"sol once\n"}}`, testBuildID, testBuildAgentID, status, log)
				default:
					t.Errorf("unexpected path: %s", r.URL)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			if err := saveLoginCredentials(srv.URL, "test@example.com", "token", ""); err != nil {
				t.Fatal(err)
			}
			output, err := captureCommandStdoutResult(t, func() error {
				return runDeployBuilds(ctx, "status", deployBuildFlags{dir: t.TempDir(), url: srv.URL, agent: "todo", watch: true, logs: true, limit: 10}, time.Millisecond)
			})
			if terminal == "cancel" {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("error=%v", err)
				}
				return
			}
			if terminal == "failed" {
				if err == nil || !strings.Contains(err.Error(), "failed") {
					t.Fatalf("error=%v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if lists != 1 || details != 4 {
				t.Fatalf("lists=%d details=%d", lists, details)
			}
			for _, want := range []string{"first\n", "second\n", "replacement\n", "sol once\n", "[Docker log snapshot reset]", "Build status: building", "Build status: " + terminal} {
				if strings.Count(output, want) != 1 {
					t.Errorf("want %q once: %s", want, output)
				}
			}
		})
	}
}

func TestDeployBuildTargetAndAuth(t *testing.T) {
	const otherID = "22222222-2222-2222-2222-222222222222"
	for _, tt := range []struct {
		name, agent, wantErr string
		code                 int
	}{
		{name: "bound default"},
		{name: "matching slug", agent: "todo"},
		{name: "conflicting slug", agent: "other", wantErr: "is bound to agent"},
		{name: "conflicting URL", wantErr: "is bound to"},
		{name: "missing login", wantErr: "not logged in"},
		{name: "integration token", wantErr: "integration token"},
		{name: "refresh"},
		{name: "forbidden", code: 403, wantErr: "403"},
		{name: "unauthorized", code: 401, wantErr: "401"},
		{name: "missing build", code: 404, wantErr: "404"},
		{name: "wrong build", wantErr: "does not match"},
		{name: "wrong agent", wantErr: "does not match"},
		{name: "missing detail", wantErr: "does not match"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("AIRLOCK_INTEGRATION_TOKEN", "")
			var paths []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.Path)
				if r.URL.Path == "/auth/refresh" {
					if tt.name != "refresh" || r.Method != "POST" {
						t.Errorf("unexpected refresh")
					}
					var request airlockv1.RefreshRequest
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
					}
					if err := protoUnmarshal.Unmarshal(body, &request); err != nil || request.RefreshToken != "refresh-token" {
						t.Errorf("refresh request: %v %+v", err, &request)
					}
					fmt.Fprint(w, `{"accessToken":"refreshed","refreshToken":"rotated"}`)
					return
				}
				wantToken := "token"
				if tt.name == "refresh" {
					wantToken = "refreshed"
				}
				if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer "+wantToken {
					t.Errorf("bad request %s %s", r.Method, r.URL)
				}
				switch r.URL.Path {
				case "/api/v1/agents/" + testBuildAgentID:
					fmt.Fprintf(w, `{"agent":{"id":%q,"slug":"todo"}}`, testBuildAgentID)
				case "/api/v1/agents":
					fmt.Fprintf(w, `{"agents":[{"id":%q,"slug":"todo"},{"id":%q,"slug":"other"}]}`, testBuildAgentID, otherID)
				case "/api/v1/agents/" + testBuildAgentID + "/builds/" + testBuildID:
					if tt.code != 0 {
						w.WriteHeader(tt.code)
						fmt.Fprint(w, `{"error":"denied"}`)
						return
					}
					if tt.name == "missing detail" {
						fmt.Fprint(w, `{}`)
						return
					}
					buildID, agentID := testBuildID, testBuildAgentID
					if tt.name == "wrong build" {
						buildID = otherID
					}
					if tt.name == "wrong agent" {
						agentID = otherID
					}
					fmt.Fprintf(w, `{"build":{"id":%q,"agentId":%q,"status":"complete"}}`, buildID, agentID)
				default:
					t.Errorf("unexpected path %s", r.URL)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()
			if tt.name != "missing login" {
				refresh := ""
				if tt.name == "refresh" {
					refresh = "refresh-token"
				}
				if err := saveLoginCredentials(srv.URL, "test@example.com", "token", refresh); err != nil {
					t.Fatal(err)
				}
			}
			dir := t.TempDir()
			binding := agentBinding{}
			binding.putRemote("prod", agentRemoteBinding{AirlockURL: srv.URL, AgentID: testBuildAgentID, Slug: "todo"})
			if err := writeAgentBinding(dir, binding); err != nil {
				t.Fatal(err)
			}
			args := []string{"status", dir, "--build", testBuildID, "--watch"}
			if tt.agent != "" {
				args = append(args, "--agent", tt.agent)
			}
			if tt.name == "conflicting URL" {
				args = append(args, "--url", "https://elsewhere.example")
			}
			if tt.name == "integration token" {
				t.Setenv("AIRLOCK_INTEGRATION_TOKEN", "codegen")
			}
			_, err := captureCommandStdoutResult(t, func() error { return cmdDeploy(args) })
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error=%v want %s", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if tt.code != 0 && !hasHTTPStatus(err, tt.code) {
				t.Errorf("lost HTTP status: %v", err)
			}
			if tt.name == "conflicting URL" || tt.name == "missing login" || tt.name == "integration token" {
				if len(paths) != 0 {
					t.Fatalf("unexpected requests: %v", paths)
				}
			}
			if tt.name == "conflicting slug" && !reflect.DeepEqual(paths, []string{"/api/v1/agents"}) {
				t.Fatalf("conflict made build requests: %v", paths)
			}
			if tt.name == "refresh" {
				creds, err := loadCredentials()
				if err != nil || creds.Sessions[srv.URL].RefreshToken != "rotated" {
					t.Fatalf("refresh not saved: %v", err)
				}
			}
		})
	}
}

func TestDeploySubcommandRoutingPreservesUploads(t *testing.T) {
	t.Setenv("AIRLOCK_INTEGRATION_TOKEN", "")
	t.Chdir(t.TempDir())
	for _, args := range [][]string{
		{"list", "-m", "message"}, {"status", "--message", "message"},
		{"-m", "list", "repo"}, {"repo", "--message", "status"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			err := cmdDeploy(args)
			if err == nil || !strings.Contains(err.Error(), "deploy requires an agent repo with go.mod") {
				t.Fatalf("not routed to upload: %v", err)
			}
		})
	}
}

func TestPrintBuildLogSnapshots(t *testing.T) {
	for _, tt := range []struct{ name, previous, current, want string }{
		{"same", "abc", "abc", ""},
		{"append", "abc", "abcdef", "[Docker log]\ndef\n"},
		{"truncate", "abcdef", "abc", "[Docker log snapshot reset]\n[Docker log]\nabc\n"},
		{"clear", "abc", "", "[Docker log snapshot reset]\n"},
		{"replace", "abc", "xyz", "[Docker log snapshot reset]\n[Docker log]\nxyz\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			out := captureCommandStdout(t, func() error { printBuildLog("Docker", tt.previous, tt.current); return nil })
			if out != tt.want {
				t.Fatalf("output=%q want=%q", out, tt.want)
			}
		})
	}
}

func TestDeployBuildWatchCancelsPollWait(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("AIRLOCK_INTEGRATION_TOKEN", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/agents/" + testBuildAgentID:
			fmt.Fprintf(w, `{"agent":{"id":%q,"slug":"todo"}}`, testBuildAgentID)
		case "/api/v1/agents/" + testBuildAgentID + "/builds/" + testBuildID:
			fmt.Fprintf(w, `{"build":{"id":%q,"agentId":%q,"status":"building"}}`, testBuildID, testBuildAgentID)
		default:
			t.Errorf("unexpected request %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	if err := saveLoginCredentials(srv.URL, "test@example.com", "token", ""); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	out, err := captureCommandStdoutResult(t, func() error {
		return runDeployBuilds(ctx, "status", deployBuildFlags{dir: t.TempDir(), url: srv.URL, agent: testBuildAgentID, build: testBuildID, watch: true}, time.Hour)
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v", err)
	}
	if strings.Count(out, "Build status: building") != 1 {
		t.Fatalf("expected one snapshot before cancellation: %s", out)
	}
}
