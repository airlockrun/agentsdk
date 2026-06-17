package agentsdk

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeAirlock is a minimal stand-in for airlock that the agentsdk client
// talks to: a /api/agent/proxy/{slug} endpoint with a tunable body size,
// and /api/agent/storage/{key} write endpoint that records what was
// spilled. Used to exercise the conn_{slug} JS bindings end-to-end.
type fakeAirlock struct {
	proxyBody  []byte
	proxyCT    string
	srv        *httptest.Server
	mu         sync.Mutex
	storageMap map[string][]byte
}

func newFakeAirlock(t *testing.T, proxyBody []byte, proxyContentType string) *fakeAirlock {
	t.Helper()
	f := &fakeAirlock{
		proxyBody:  proxyBody,
		proxyCT:    proxyContentType,
		storageMap: map[string][]byte{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/agent/proxy/{slug}", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if f.proxyCT != "" {
			w.Header().Set("Content-Type", f.proxyCT)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(f.proxyBody)
	})
	mux.HandleFunc("PUT /api/agent/storage/{key...}", func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/api/agent/storage/")
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.storageMap[key] = body
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAirlock) stored(key string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.storageMap[key]
}

func (f *fakeAirlock) storageWriteCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.storageMap)
}

func setupConnAgent(t *testing.T, f *fakeAirlock, connSlug string) (*Agent, *run) {
	t.Helper()
	a := &Agent{
		agentID:          "test-agent",
		apiURL:           f.srv.URL,
		token:            "test-token",
		httpClient:       &http.Client{},
		sensitiveSet:     make(map[string]struct{}),
		tools:            make(map[string]*registeredTool),
		webhooks:         make(map[string]*Webhook),
		scheduleHandlers: make(map[string]*scheduleHandler),
		auths:            make(map[string]*Connection),
		mcps:             make(map[string]*MCP),
		topics:           make(map[string]*Topic),
		routes:           make(map[string]*Route),
		execEndpoints:    make(map[string]*ExecEndpoint),
	}
	a.client = newAirlockClient(f.srv.URL, "test-token", a.httpClient)
	a.auths[connSlug] = &Connection{Slug: connSlug, Access: AccessAdmin}
	r := newRun(a, "run-1", "", "", context.Background())
	return a, r
}

func TestVMBinding_ConnRequest_InlineUnderThreshold(t *testing.T) {
	body := bytes.Repeat([]byte("a"), 100)
	f := newFakeAirlock(t, body, "text/plain")
	_, r := setupConnAgent(t, f, "x")

	out, err := executeJS(r.vmRuntime(), `JSON.stringify(conn_x.request("GET", "/whatever"))`)
	if err != nil {
		t.Fatalf("executeJS: %v", err)
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(unquoteJSON(out)), &env); err != nil {
		t.Fatalf("unmarshal envelope: %v (raw: %s)", err, out)
	}
	if env["body"] == nil {
		t.Errorf("body absent on inline result: %v", env)
	}
	if env["bodySavedTo"] != nil {
		t.Errorf("bodySavedTo set on inline result: %v", env["bodySavedTo"])
	}
	if f.storageWriteCount() != 0 {
		t.Errorf("storage written %d times on inline path; want 0", f.storageWriteCount())
	}
}

func TestVMBinding_ConnRequest_SpillsAboveThreshold(t *testing.T) {
	body := bytes.Repeat([]byte("b"), 50*1024)
	f := newFakeAirlock(t, body, "application/octet-stream")
	_, r := setupConnAgent(t, f, "x")

	out, err := executeJS(r.vmRuntime(), `JSON.stringify(conn_x.request("GET", "/big"))`)
	if err != nil {
		t.Fatalf("executeJS: %v", err)
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(unquoteJSON(out)), &env); err != nil {
		t.Fatalf("unmarshal: %v (raw: %s)", err, out)
	}
	if env["body"] != nil {
		t.Errorf("body must be absent when spilled: %v", env["body"])
	}
	savedTo, _ := env["bodySavedTo"].(string)
	if !strings.HasPrefix(savedTo, "tmp/conn-x-") || !strings.HasSuffix(savedTo, ".bin") {
		t.Errorf("bodySavedTo = %q, want tmp/conn-x-{callID}.bin", savedTo)
	}
	preview, _ := env["bodyPreview"].(string)
	if len(preview) != spillPreviewBytes {
		t.Errorf("bodyPreview = %d bytes, want %d", len(preview), spillPreviewBytes)
	}
	if got, _ := env["size"].(float64); int(got) != len(body) {
		t.Errorf("size = %v, want %d", env["size"], len(body))
	}
	if stored := f.stored(savedTo); len(stored) != len(body) {
		t.Errorf("stored %d bytes, want %d", len(stored), len(body))
	}
}

func TestVMBinding_ConnRequestJSON_InlineParsedToData(t *testing.T) {
	body := []byte(`{"hello":"world"}`)
	f := newFakeAirlock(t, body, "application/json")
	_, r := setupConnAgent(t, f, "x")

	out, err := executeJS(r.vmRuntime(),
		`var r = conn_x.requestJSON("GET", "/j"); JSON.stringify({data: r.data, status: r.status})`)
	if err != nil {
		t.Fatalf("executeJS: %v", err)
	}
	var got struct {
		Data   map[string]string `json:"data"`
		Status int               `json:"status"`
	}
	if err := json.Unmarshal([]byte(unquoteJSON(out)), &got); err != nil {
		t.Fatalf("unmarshal: %v (raw: %s)", err, out)
	}
	if got.Data["hello"] != "world" {
		t.Errorf("data.hello = %q, want world", got.Data["hello"])
	}
	if got.Status != 200 {
		t.Errorf("status = %d, want 200", got.Status)
	}
}

func TestVMBinding_ConnRequestJSON_OverflowOmitsData(t *testing.T) {
	// Build a large JSON-shaped body that exceeds the threshold.
	body := append([]byte(`{"items":["`), bytes.Repeat([]byte("x"), 50*1024)...)
	body = append(body, []byte(`"]}`)...)
	f := newFakeAirlock(t, body, "application/json")
	_, r := setupConnAgent(t, f, "x")

	out, err := executeJS(r.vmRuntime(),
		`JSON.stringify(conn_x.requestJSON("GET", "/big"))`)
	if err != nil {
		t.Fatalf("executeJS: %v", err)
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(unquoteJSON(out)), &env); err != nil {
		t.Fatalf("unmarshal: %v (raw: %s)", err, out)
	}
	if env["data"] != nil {
		t.Errorf("data must be absent on overflow; got %v", env["data"])
	}
	savedTo, _ := env["bodySavedTo"].(string)
	if savedTo == "" {
		t.Fatalf("bodySavedTo missing; envelope=%v", env)
	}
	note, _ := env["note"].(string)
	if !strings.Contains(note, "JSON.parse(fileRead") {
		t.Errorf("requestJSON overflow note must point at JSON.parse(fileRead); got %q", note)
	}
}

// unquoteJSON peels one layer of JSON-string quoting from a result that
// came back as a string (executeJS already JSON.stringified once). For
// expressions like `JSON.stringify(obj)`, formatJSValue stringifies the
// already-stringified string, double-encoding it. We need to undo one
// layer for json.Unmarshal of the inner object.
func unquoteJSON(s string) string {
	var out string
	if err := json.Unmarshal([]byte(s), &out); err == nil {
		return out
	}
	return s
}
