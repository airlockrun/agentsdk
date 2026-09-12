package agentsdk

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/airlockrun/agentsdk/capability"
	"github.com/airlockrun/agentsdk/wire"
	"github.com/airlockrun/goai/tool"
	"github.com/google/uuid"
)

func runtimeRequest(a *Agent, id string) wire.RuntimeInvokeRequest {
	return wire.RuntimeInvokeRequest{
		RuntimeProtocol: wire.AppRuntimeProtocol,
		CapabilityID:    id, ToolCallID: "call-1", Input: json.RawMessage(`{}`),
		Context: wire.RuntimeContext{AgentID: a.agentID, RunID: uuid.NewString(), Caller: testWireCaller("user", wire.AccessUser), InvocationToken: strings.Repeat("ab", 32)},
	}
}

func invokeRuntime(t *testing.T, handler http.Handler, req wire.RuntimeInvokeRequest, token string) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, wire.RuntimeInvokePath, bytes.NewReader(data))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	r.Header.Set("X-Caller-Access", string(AccessAdmin))
	r.Header.Set("X-User-ID", uuid.NewString())
	r.Header.Set("X-Run-ID", uuid.NewString())
	r.Header[wire.CallerHeader] = []string{"invalid", "ignored"}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

func TestRuntimeInvokeCallerKinds(t *testing.T) {
	for _, kind := range []string{"anonymous", "user", "application"} {
		t.Run(kind, func(t *testing.T) {
			a, mock := testAgent(t)
			req := runtimeRequest(a, "tool//caller")
			req.Context.Caller = testWireCaller(kind, wire.AccessPublic)
			req.Context.Caller.Origin = wire.CallerOrigin{Interface: "mcp", Platform: "test-client", ClientID: "oauth-client", Execution: "request"}
			a.RegisterTool(tool.New("caller").Description("Read caller").Execute(func(ctx context.Context, _ json.RawMessage, _ tool.CallOptions) (tool.Result, error) {
				if CallerFromContext(ctx) != callerFromWire(req.Context.Caller) {
					t.Fatal("lost body caller")
				}
				return tool.Result{Output: "ok"}, nil
			}).Build(), AccessPublic)
			w := invokeRuntime(t, a.Handler(), req, a.token)
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if len(mock.Requests()) != 0 {
				t.Fatal("caller lookup performed I/O")
			}
		})
	}
}

func TestRuntimeInvokeAuthorization(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*wire.RuntimeInvokeRequest)
		token  string
		status int
	}{
		{"missing auth", func(*wire.RuntimeInvokeRequest) {}, "", 401},
		{"wrong agent token", func(*wire.RuntimeInvokeRequest) {}, "another-token", 401},
		{"missing protocol", func(r *wire.RuntimeInvokeRequest) { r.RuntimeProtocol = "" }, "test-token", 409},
		{"old protocol", func(r *wire.RuntimeInvokeRequest) { r.RuntimeProtocol = "airlock.app-runtime.v1" }, "test-token", 409},
		{"unknown protocol", func(r *wire.RuntimeInvokeRequest) { r.RuntimeProtocol = "airlock.app-runtime.v3" }, "test-token", 409},
		{"wrong agent scope", func(r *wire.RuntimeInvokeRequest) { r.Context.AgentID = "other-agent" }, "test-token", 403},
		{"missing run", func(r *wire.RuntimeInvokeRequest) { r.Context.RunID = "" }, "test-token", 400},
		{"invalid run", func(r *wire.RuntimeInvokeRequest) { r.Context.RunID = "not-a-run" }, "test-token", 400},
		{"missing caller", func(r *wire.RuntimeInvokeRequest) { r.Context.Caller = wire.Caller{} }, "test-token", 400},
		{"missing access", func(r *wire.RuntimeInvokeRequest) { r.Context.Caller.Access = "" }, "test-token", 400},
		{"invalid access", func(r *wire.RuntimeInvokeRequest) { r.Context.Caller.Access = "superuser" }, "test-token", 400},
		{"registration access", func(r *wire.RuntimeInvokeRequest) { r.Context.Caller.Access = wire.AccessPublic }, "test-token", 403},
		{"unknown capability", func(r *wire.RuntimeInvokeRequest) { r.CapabilityID = "tool//missing" }, "test-token", 404},
		{"direct alias is not identity", func(r *wire.RuntimeInvokeRequest) { r.CapabilityID = "tool__check" }, "test-token", 404},
		{"platform does not execute in app", func(r *wire.RuntimeInvokeRequest) { r.CapabilityID = "air//http_request" }, "test-token", 404},
		{"DB requires admin", func(r *wire.RuntimeInvokeRequest) { r.CapabilityID = "air//query_db" }, "test-token", 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, mock := testAgent(t)
			a.RegisterTool(tool.New("check").Description("Check access").Execute(func(context.Context, json.RawMessage, tool.CallOptions) (tool.Result, error) {
				t.Fatal("unauthorized call executed")
				return tool.Result{}, nil
			}).Build(), AccessUser)
			req := runtimeRequest(a, "tool//check")
			tc.change(&req)
			w := invokeRuntime(t, a.Handler(), req, tc.token)
			if w.Code != tc.status {
				t.Fatalf("status = %d: %s", w.Code, w.Body.String())
			}
			if len(mock.Requests()) != 0 {
				t.Fatalf("denied invocation made API requests: %v", mock.Requests())
			}
		})
	}
}

func TestRuntimeInvokeBorrowsRunAndPreservesResult(t *testing.T) {
	a, mock := testAgent(t)
	req := runtimeRequest(a, "tool//check")
	req.Context.Caller.User.ID = uuid.NewString()
	req.Context.ConversationID, req.Context.BridgeID = uuid.NewString(), uuid.NewString()
	req.Context.Caller.Origin = wire.CallerOrigin{Interface: "chat", Platform: "web", ClientID: "browser", Execution: "request"}
	req.Context.Job = &wire.RuntimeJobContext{ID: uuid.NewString(), Attempt: 2, LeaseToken: uuid.NewString()}
	req.Input = json.RawMessage(`{"number":42}`)
	a.RegisterTool(tool.New("check").Description("Check context").Execute(func(ctx context.Context, input json.RawMessage, opts tool.CallOptions) (tool.Result, error) {
		if AgentFromContext(ctx) != a {
			t.Fatal("missing agent")
		}
		active := runFromContext(ctx)
		if active.id != req.Context.RunID || active.conversationID != req.Context.ConversationID || active.bridgeID != req.Context.BridgeID || CallerFromContext(ctx).Origin().Platform != "web" {
			t.Fatalf("lost run context: %+v", active)
		}
		user, ok := CallerFromContext(ctx).User()
		if !ok || user != User(*req.Context.Caller.User) {
			t.Fatalf("lost user: %+v", user)
		}
		if caller := callScopeFromContext(ctx); caller.Access != AccessUser || caller.UserID != user.ID || caller.RunID != active.id {
			t.Fatalf("lost caller: %+v", caller)
		}
		job := jobRunFromContext(ctx)
		if job == nil || job.id != req.Context.Job.ID || job.attempt != 2 || job.leaseToken != req.Context.Job.LeaseToken {
			t.Fatalf("lost job: %+v", job)
		}
		if opts.ToolCallID != req.ToolCallID || opts.AbortSignal != ctx || !bytes.Equal(input, req.Input) {
			t.Fatal("changed execute signature/arguments")
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("missing bounded context")
		}
		active.logAppend(wire.LogLevelInfo, "test-token log "+job.leaseToken)
		active.recordAction("test", map[string]any{"secret": "test-token"}, nil, nil, 0)
		return tool.Result{Output: `{"value":"test-token","number":9007199254740993}`, Title: "test-token title", Metadata: map[string]any{"nested": map[string]any{"secret": "test-token"}}, Attachments: []tool.Attachment{
			{Data: "s3ref:tmp/existing.png", MimeType: "image/png", Filename: "image.png"},
			{Data: base64.StdEncoding.EncodeToString([]byte("file contents")), MimeType: "text/plain", Filename: "note.txt"},
			{Data: "invalid!", MimeType: "text/plain"},
		}}, nil
	}).Build(), AccessUser)
	w := invokeRuntime(t, a.Handler(), req, a.token)
	if w.Code != 200 {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if w.Header().Get("Content-Type") != "application/json" {
		t.Fatal("not a typed JSON result")
	}
	if strings.Contains(w.Body.String(), "test-token") || strings.Contains(w.Body.String(), req.Context.Job.LeaseToken) || strings.Contains(w.Body.String(), "file contents") || strings.Contains(w.Body.String(), "ZmlsZSBjb250ZW50cw==") {
		t.Fatalf("secret or attachment bytes escaped: %s", w.Body.String())
	}
	var response wire.RuntimeInvokeResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Title != "[REDACTED] title" || !strings.Contains(response.Output, "9007199254740993") || len(response.Metadata) != 1 || len(response.Attachments) != 2 || len(response.Warnings) != 1 || len(response.Logs) != 1 || len(response.Actions) != 1 {
		t.Fatalf("lost result fields: %+v", response)
	}
	if response.Attachments[0].Path != "tmp/existing.png" || response.Attachments[0].Filename != "image.png" || !strings.HasPrefix(response.Attachments[1].Path, "tmp/attachments/") {
		t.Fatalf("bad references: %+v", response.Attachments)
	}
	requests := mock.Requests()
	if len(requests) != 1 || requests[0].Method != "PUT" || !strings.Contains(requests[0].Path, "/storage/tmp/attachments/") {
		t.Fatalf("expected only attachment storage, no run creation/completion: %+v", requests)
	}
}

func TestRuntimeInvokeFailureAndCancellation(t *testing.T) {
	for _, name := range []string{"error", "panic", "cancel"} {
		t.Run(name, func(t *testing.T) {
			a, mock := testAgent(t)
			a.RegisterTool(tool.New("check").Description("Fail").Execute(func(ctx context.Context, _ json.RawMessage, _ tool.CallOptions) (tool.Result, error) {
				runFromContext(ctx).logAppend(wire.LogLevelWarn, "before failure")
				switch name {
				case "panic":
					panic("test-token")
				case "cancel":
					<-ctx.Done()
					return tool.Result{}, ctx.Err()
				default:
					return tool.Result{Title: "partial"}, errors.New("test-token failed")
				}
			}).Build(), AccessUser)
			req := runtimeRequest(a, "tool//check")
			handler := a.Handler()
			if name == "cancel" {
				inner := handler
				handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					ctx, cancel := context.WithCancel(r.Context())
					cancel()
					inner.ServeHTTP(w, r.WithContext(ctx))
				})
			}
			w := invokeRuntime(t, handler, req, a.token)
			var result wire.RuntimeInvokeResponse
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if w.Code != 200 || result.Error == "" || len(result.Logs) != 1 || strings.Contains(w.Body.String(), "test-token") {
				t.Fatalf("failure not preserved safely: %s", w.Body.String())
			}
			if len(mock.Requests()) != 0 {
				t.Fatal("borrowed run was created or completed")
			}
		})
	}
}

func TestRuntimeInvokeFileScope(t *testing.T) {
	for _, access := range []wire.Access{wire.AccessPublic, wire.AccessUser} {
		t.Run(string(access), func(t *testing.T) {
			a, mock := testAgent(t)
			req := runtimeRequest(a, "air//file_read")
			req.Context.Caller.Access = access
			req.Input = json.RawMessage(`{"path":"tmp/test.txt"}`)
			w := invokeRuntime(t, a.Handler(), req, a.token)
			var response wire.RuntimeInvokeResponse
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if access == wire.AccessPublic {
				if response.Error == "" || len(mock.Requests()) != 0 {
					t.Fatal("public file scope bypass")
				}
			} else if response.Output != "mock-file-content" || response.Error != "" {
				t.Fatalf("file read changed: %+v", response)
			}
		})
	}
}

func TestRuntimeInventory(t *testing.T) {
	a, _ := testAgent(t)
	r := newRun(a, uuid.NewString(), "", "", context.Background())
	local := runtimeLocalTools(a, r)
	for _, d := range capability.Fixed() {
		if d.Path.Kind() != capability.Air {
			continue
		}
		if d.Target == capability.App {
			executor, ok := local[d.Path.CanonicalOperation()]
			if !ok {
				t.Fatalf("missing local executor: %s", d.Path.ID())
			}
			if !bytes.Equal(executor.InputSchema, d.InputSchema) {
				t.Fatalf("catalogue/executor schema drift: %s\ncatalogue: %s\nexecutor: %s", d.Path.ID(), d.InputSchema, executor.InputSchema)
			}
			delete(local, d.Path.CanonicalOperation())
		}
	}
	if len(local) != 0 {
		t.Fatalf("uncatalogued capabilities: local=%v", reflect.ValueOf(local).MapKeys())
	}
}

func TestRuntimeInvokeJobProgressAttribution(t *testing.T) {
	a, mock := testAgent(t)
	req := runtimeRequest(a, "tool//progress")
	req.Context.Job = &wire.RuntimeJobContext{ID: uuid.NewString(), Attempt: 3, LeaseToken: uuid.NewString()}
	a.RegisterTool(tool.New("progress").Description("Report job progress").Execute(func(ctx context.Context, _ json.RawMessage, _ tool.CallOptions) (tool.Result, error) {
		job := JobContext{ID: req.Context.Job.ID, Attempt: req.Context.Job.Attempt}
		return tool.Result{Output: "reported"}, job.ReportProgress(ctx, JobProgress{Phase: "work", Completed: 1, Total: 2})
	}).Build(), AccessUser)
	w := invokeRuntime(t, a.Handler(), req, a.token)
	if w.Code != 200 {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	requests := mock.Requests()
	if len(requests) != 1 || requests[0].Path != "/api/agent/jobs/"+req.Context.Job.ID+"/progress" || requests[0].Header.Get(jobLeaseTokenHeader) != req.Context.Job.LeaseToken || requests[0].Header.Get("X-Airlock-Run-ID") != req.Context.RunID {
		t.Fatalf("lost outbound job/run attribution: %+v", requests)
	}
}

func TestRuntimeResponseRedaction(t *testing.T) {
	a, _ := testAgent(t)
	secret := "credential\"with\nquotes"
	a.AddSensitive(secret, "overlap", "overlap-complete-secret")
	output, err := json.Marshal(map[string]any{"secret": secret, "overlap": "overlap-complete-secret"})
	if err != nil {
		t.Fatal(err)
	}
	response := wire.RuntimeInvokeResponse{Output: string(output), Metadata: map[string]any{secret: []any{secret}}, Error: secret, Title: secret, Warnings: []string{secret}, Logs: []wire.LogEntry{{Message: secret}}}
	data, err := a.marshalRuntimeResponse(response, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(data) || strings.Contains(string(data), "credential") || strings.Contains(string(data), "complete-secret") {
		t.Fatalf("redaction failed: %s", data)
	}
	if err := json.Unmarshal(data, &response); err != nil {
		t.Fatal(err)
	}
	if !json.Valid([]byte(response.Output)) {
		t.Fatalf("redaction corrupted nested output: %s", response.Output)
	}
	const unchanged = "{ \"value\": 42 }"
	data, err = a.marshalRuntimeResponse(wire.RuntimeInvokeResponse{Output: unchanged}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &response); err != nil {
		t.Fatal(err)
	}
	if response.Output != unchanged {
		t.Fatal("non-sensitive output was rewritten")
	}
	a.AddSensitive("output", "message")
	data, err = a.marshalRuntimeResponse(wire.RuntimeInvokeResponse{Output: "output", Logs: []wire.LogEntry{{Level: wire.LogLevelInfo, Message: "message"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatal(err)
	}
	if _, ok := envelope["output"]; !ok {
		t.Fatal("redaction renamed a protocol field")
	}
	if err := json.Unmarshal(data, &response); err != nil {
		t.Fatal(err)
	}
	if response.Output != "[REDACTED]" || response.Logs[0].Message != "[REDACTED]" {
		t.Fatalf("content not redacted: %s", data)
	}
}

func TestRuntimeInvokeRejectsMalformedProtocol(t *testing.T) {
	a, _ := testAgent(t)
	valid, err := json.Marshal(runtimeRequest(a, "air//file_read"))
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{string(valid) + ` {}`, `{"catalog":[]}`, strings.Repeat(" ", maxRuntimeInvokeBytes) + string(valid)} {
		r := httptest.NewRequest("POST", wire.RuntimeInvokePath, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+a.token)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatalf("malformed protocol status = %d", w.Code)
		}
	}
}
