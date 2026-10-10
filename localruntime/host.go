package localruntime

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/airlockrun/agentsdk/capability"
	"github.com/airlockrun/agentsdk/chatruntime"
	"github.com/airlockrun/agentsdk/wire"
	"github.com/airlockrun/goai/stream"
	"github.com/airlockrun/sol/session"
	"github.com/google/uuid"
	"github.com/robfig/cron/v3"
)

func readBody(r *http.Request) ([]byte, error) {
	raw, err := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(raw))
	return raw, err
}

type ModelBinding struct {
	Model  stream.Model
	Limits session.ModelLimits
}

// Config supplies local host dependencies. AppURL addresses the real SDK app
// listener; its client may use an injected transport in isolated tests.
type Config struct {
	AppID           string
	Token           string
	AppURL          string
	RouteURL        string
	State           *State
	Storage         *FileStorage
	Models          map[string]ModelBinding
	Client          *http.Client
	ExecutorFactory chatruntime.ExecutorFactory
	Discovery       capability.Discovery
	Platform        chatruntime.Backend
	Resources       Resources
	NoAuthResources Resources
	EnvVars         map[string]string
	// ResourceSession is the verified remote session coordinate, never a token.
	ResourceSession string
}

type Host struct {
	config  Config
	handler http.Handler
}

func New(config Config) (*Host, error) {
	id, err := uuid.Parse(config.AppID)
	if err != nil || id.String() != config.AppID {
		return nil, errors.New("localruntime: canonical app UUID is required")
	}
	if config.Token == "" || config.AppURL == "" || config.RouteURL == "" || config.State == nil || config.Storage == nil || config.Client == nil || config.ExecutorFactory == nil {
		return nil, errors.New("localruntime: token, app URL, state, storage, HTTP client and executor factory are required")
	}
	bindings := map[string]stream.Model{}
	copied := map[string]ModelBinding{}
	for slot, binding := range config.Models {
		if binding.Model == nil {
			return nil, fmt.Errorf("localruntime: slot %q model is required", slot)
		}
		if err := binding.Limits.Validate(true); err != nil {
			return nil, fmt.Errorf("localruntime: slot %q: %w", slot, err)
		}
		bindings[slot] = binding.Model
		copied[slot] = binding
	}
	config.Models = copied
	config.EnvVars = clone(config.EnvVars)
	if config.Resources != nil && config.ResourceSession == "" {
		return nil, errors.New("localruntime: resource adapter requires an explicit verified session coordinate")
	}
	if err := config.State.update(context.Background(), func(d *stateData) error {
		if d.AppID != config.AppID {
			return errors.New("localruntime: host and persisted app identity mismatch")
		}
		return nil
	}); err != nil {
		return nil, err
	}
	h := &Host{config: config}
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/agent/sync", h.sync)
	mux.HandleFunc("GET /api/agent/env-vars/{slug}", h.envVar)
	mux.Handle("POST /api/agent/proxy/{slug}", h.resourceProof(http.HandlerFunc(h.proxyConnection)))
	mux.Handle("POST /api/agent/mcp/{slug}/tools/call", h.resourceProof(http.HandlerFunc(h.callMCP)))
	mux.Handle("POST /api/agent/llm/stream", h.receipt(ModelHandler(bindings)))
	for pattern, op := range map[string]string{
		"PUT /api/agent/storage/{key...}": "put", "GET /api/agent/storage/{key...}": "get", "DELETE /api/agent/storage/{key...}": "delete",
		"GET /api/agent/storage": "list", "POST /api/agent/storage/info": "info", "POST /api/agent/storage/copy": "copy",
	} {
		mux.Handle(pattern, h.storage(StorageHandler(config.Storage, op), op))
	}
	mux.HandleFunc("POST /api/agent/run/create", h.createRun)
	mux.Handle("POST /api/agent/run/complete", h.receipt(http.HandlerFunc(h.completeRun)))
	mux.HandleFunc("POST /api/agent/agents/{definition}/runs", h.startTask)
	mux.HandleFunc("GET /api/agent/agents/{definition}/runs", h.listTasks)
	mux.HandleFunc("GET /api/agent/agents/{definition}/runs/{id}", h.getTask)
	mux.HandleFunc("DELETE /api/agent/agents/{definition}/runs/{id}", h.cancelTask)
	mux.HandleFunc("POST /api/agent/agents/{definition}/sessions/{session}/continue", h.continueTask)
	mux.Handle("POST /api/agent/jobs", h.receipt(http.HandlerFunc(h.enqueueJob)))
	mux.HandleFunc("GET /api/agent/jobs/{id}", h.getJob)
	mux.HandleFunc("DELETE /api/agent/jobs/{id}", h.cancelJob)
	mux.HandleFunc("PUT /api/agent/jobs/{id}/progress", h.progressJob)
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "localruntime: this host operation requires a configured local adapter", http.StatusNotImplemented)
	}))
	h.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.Header.Values("Authorization")) != 1 || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+config.Token)) != 1 {
			http.Error(w, "unauthorized", 401)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 80<<20)
		mux.ServeHTTP(w, r)
	})
	return h, nil
}

func (h *Host) Handler() http.Handler { return h.handler }

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
func fail(w http.ResponseWriter, err error) { http.Error(w, err.Error(), http.StatusConflict) }

func (h *Host) sync(w http.ResponseWriter, r *http.Request) {
	var manifest wire.AgentManifest
	if err := strictJSON(r.Body, &manifest); err != nil {
		fail(w, err)
		return
	}
	if err := wire.CheckAppRuntimeProtocol(manifest.RuntimeProtocol); err != nil {
		fail(w, err)
		return
	}
	if err := wire.ValidateAgentDefinitions(manifest); err != nil {
		fail(w, err)
		return
	}
	for _, slot := range manifest.ModelSlots {
		if _, ok := h.config.Models[slot.Slug]; !ok {
			fail(w, fmt.Errorf("localruntime: model slot %q needs an explicit local binding", slot.Slug))
			return
		}
		if slot.Capability != "text" && slot.Capability != "vision" {
			fail(w, fmt.Errorf("localruntime: slot %q needs a %s model adapter", slot.Slug, slot.Capability))
			return
		}
	}
	for _, c := range manifest.JobCrons {
		if _, err := cron.ParseStandard(c.Schedule); err != nil {
			fail(w, fmt.Errorf("localruntime: cron %q: %w", c.Slug, err))
			return
		}
	}
	for slug := range h.config.EnvVars {
		found := false
		for _, decl := range manifest.EnvVars {
			if decl.Slug == slug {
				found = true
				break
			}
		}
		if !found {
			fail(w, fmt.Errorf("localruntime: environment slot %q is not declared", slug))
			return
		}
	}
	err := h.config.State.update(r.Context(), func(d *stateData) error {
		retained := map[string]time.Time{}
		for _, c := range manifest.JobCrons {
			next := d.Crons[c.Slug]
			same := false
			for _, old := range d.Manifest.JobCrons {
				if old.Slug == c.Slug && old.Schedule == c.Schedule {
					same = true
				}
			}
			if next.IsZero() || !same {
				schedule, _ := cron.ParseStandard(c.Schedule)
				next = schedule.Next(time.Now())
			}
			retained[c.Slug] = next
		}
		d.Crons = retained
		d.Manifest = manifest
		return nil
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, wire.SyncResponse{RuntimeProtocol: wire.AppRuntimeProtocol, PromptData: wire.PromptData{AgentRouteURL: h.config.RouteURL}, MCPSchemas: h.config.Discovery.MCPSchemas})
}

func newScope(appID string, caller wire.Caller) wire.RuntimeContext {
	var token [32]byte
	if _, err := rand.Read(token[:]); err != nil {
		panic(err)
	}
	return wire.RuntimeContext{AgentID: appID, RunID: uuid.NewString(), InvocationToken: hex.EncodeToString(token[:]), Caller: caller}
}

func appCaller(execution string) wire.Caller {
	return wire.Caller{Kind: "application", Access: wire.AccessAdmin, Origin: wire.CallerOrigin{Interface: "application", Execution: execution}}
}

func (h *Host) Admit(ctx context.Context, caller wire.Caller) (wire.RuntimeContext, error) {
	if err := caller.Validate(); err != nil {
		return wire.RuntimeContext{}, err
	}
	scope := newScope(h.config.AppID, caller)
	err := h.config.State.update(ctx, func(d *stateData) error {
		record := &runRecord{Scope: scope, ExpiresAt: time.Now().Add(15 * time.Minute)}
		if caller.Kind == "user" {
			record.ResourceSession = h.config.ResourceSession
		}
		d.Runs[scope.RunID] = record
		return nil
	})
	return scope, err
}

// CloseInvocation closes the exact ingress scope after app dispatch drains.
func (h *Host) CloseInvocation(ctx context.Context, id string) error {
	return h.config.State.update(ctx, func(d *stateData) error {
		run := d.Runs[id]
		if run == nil {
			return errors.New("localruntime: invocation not found")
		}
		run.Closed = true
		return nil
	})
}

func (h *Host) createRun(w http.ResponseWriter, r *http.Request) {
	// Native app callbacks cannot assert a human. HTTP ingress calls Admit with
	// the explicit developer identity before constructing an app request.
	var req wire.CreateRunRequest
	if err := strictJSON(r.Body, &req); err != nil {
		fail(w, err)
		return
	}
	caller := appCaller("background")
	if req.TriggerRef == "startup" {
		caller = appCaller("startup")
	}
	scope, err := h.Admit(r.Context(), caller)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, wire.CreateRunResponse{RunID: scope.RunID, InvocationToken: scope.InvocationToken, Caller: scope.Caller})
}

func (h *Host) receipt(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Airlock-Run-ID")
		token := r.Header.Get(wire.InvocationTokenHeader)
		err := h.config.State.update(r.Context(), func(d *stateData) error {
			run := d.Runs[id]
			if run == nil || run.Closed || token == "" || len(r.Header.Values(wire.InvocationTokenHeader)) != 1 || subtle.ConstantTimeCompare([]byte(run.Scope.InvocationToken), []byte(token)) != 1 {
				return errors.New("localruntime: invalid or closed run receipt")
			}
			if task := d.Tasks[id]; task != nil && (task.Info.Status != "running" || !task.LeaseUntil.After(time.Now())) {
				return errors.New("localruntime: task receipt owner is not live")
			}
			if run.Scope.Job != nil {
				job := d.Jobs[run.Scope.Job.ID]
				if job == nil || job.Info.Status != "running" || job.Owner != run.Scope.Job.LeaseToken || !job.LeaseUntil.After(time.Now()) {
					return errors.New("localruntime: job receipt owner is not live")
				}
			} else if d.Tasks[id] == nil && !run.ExpiresAt.After(time.Now()) {
				return errors.New("localruntime: invocation receipt expired")
			}
			return nil
		})
		if err != nil {
			http.Error(w, "localruntime: invalid or closed run receipt", 403)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Host) completeRun(w http.ResponseWriter, r *http.Request) {
	var completion json.RawMessage
	if err := strictJSON(r.Body, &completion); err != nil {
		fail(w, err)
		return
	}
	err := h.config.State.update(r.Context(), func(d *stateData) error {
		run := d.Runs[r.Header.Get("X-Airlock-Run-ID")]
		run.Closed = true
		run.Completion = completion
		return nil
	})
	if err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(204)
}

func (h *Host) storage(next http.Handler, op string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Native file methods are trusted app capabilities. Directory/scoped caller
		// policy for untrusted tools is enforced by the actual SDK invocation handler.
		p := r.PathValue("key")
		if op == "list" {
			p = strings.TrimSuffix(r.URL.Query().Get("path"), "/")
		}
		var paths []string
		if op == "info" || op == "copy" {
			var args struct {
				Path string `json:"path,omitempty"`
				Src  string `json:"src,omitempty"`
				Dst  string `json:"dst,omitempty"`
			}
			raw, err := readBody(r)
			if err != nil {
				fail(w, err)
				return
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				fail(w, err)
				return
			}
			if op == "info" {
				paths = []string{args.Path}
			} else {
				paths = []string{args.Src, args.Dst}
			}
		} else {
			paths = []string{p}
		}
		err := h.config.State.update(r.Context(), func(d *stateData) error {
			for _, p := range paths {
				if p == "" && op == "list" {
					continue
				}
				found := false
				for _, dir := range d.Manifest.Directories {
					root := strings.TrimSuffix(dir.Path, "/")
					if p == root || strings.HasPrefix(p, root+"/") {
						found = true
						break
					}
				}
				if !found {
					return errors.New("localruntime: storage path is outside registered directories")
				}
			}
			return nil
		})
		if err != nil {
			http.Error(w, err.Error(), 403)
			return
		}
		next.ServeHTTP(w, r)
	})
}
