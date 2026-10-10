package localruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/airlockrun/agentsdk/agentruntime"
	"github.com/airlockrun/agentsdk/capability"
	"github.com/airlockrun/agentsdk/wire"
	"github.com/google/uuid"
)

func definition(d *stateData, slug, hash string) (wire.AgentDefinition, error) {
	for _, def := range d.Manifest.AgentDefinitions {
		if def.Slug == slug && def.ContractHash == hash {
			return def, nil
		}
	}
	return wire.AgentDefinition{}, errors.New("localruntime: definition contract is not registered")
}

func (h *Host) enqueue(d *stateData, slug string, req wire.StartAgentRequest, sessionID, parent string) (wire.AgentRunResponse, error) {
	if id, err := uuid.Parse(req.RequestID); err != nil || id == uuid.Nil || id.String() != req.RequestID {
		return wire.AgentRunResponse{}, errors.New("localruntime: canonical request UUID is required")
	}
	def, err := definition(d, slug, req.ContractHash)
	if err != nil {
		return wire.AgentRunResponse{}, err
	}
	if err := agentruntime.ValidateJSON(def.InputSchema, req.Input); err != nil && sessionID == "" {
		return wire.AgentRunResponse{}, err
	}
	for _, task := range d.Tasks {
		if task.RequestID == req.RequestID && task.Info.Definition == slug {
			if task.Input != string(req.Input) || task.Info.ContractHash != req.ContractHash || task.ParentID != parent || sessionID != "" && task.Info.SessionID != sessionID {
				return wire.AgentRunResponse{}, errors.New("localruntime: request ID payload conflict")
			}
			return wire.AgentRunResponse{Run: clone(task.Info)}, nil
		}
		if sessionID != "" && task.Info.SessionID == sessionID && task.Info.CompletedAt == nil {
			return wire.AgentRunResponse{}, errors.New("localruntime: session has an active call")
		}
	}
	if parent != "" {
		root := d.Tasks[parent]
		if root == nil || root.ParentID != "" {
			return wire.AgentRunResponse{}, errors.New("localruntime: child requires a top-level parent")
		}
		parentDef, err := definition(d, root.Info.Definition, root.Info.ContractHash)
		if err != nil {
			return wire.AgentRunResponse{}, err
		}
		calls, active := int32(0), int32(0)
		for _, child := range d.Tasks {
			if child.ParentID == parent {
				calls++
				if child.Info.CompletedAt == nil {
					active++
				}
			}
		}
		if calls >= parentDef.MaxSubagentCalls || active >= parentDef.MaxConcurrentSubagents {
			return wire.AgentRunResponse{}, errors.New("localruntime: child call budget exceeded")
		}
	}
	now := time.Now().UTC()
	scope := newScope(h.config.AppID, appCaller("background"))
	scope.Definition = &wire.RuntimeAgentDefinition{Slug: def.Slug, ContractHash: def.ContractHash}
	if sessionID == "" {
		sessionID = uuid.NewString()
	}
	scope.ConversationID = sessionID
	record := &taskRecord{Info: wire.AgentRunInfo{ID: scope.RunID, SessionID: sessionID, Definition: slug, ContractHash: req.ContractHash, Status: "queued", CreatedAt: now, UpdatedAt: now}, Scope: scope, RequestID: req.RequestID, Input: string(req.Input), ParentID: parent}
	if parent != "" {
		record.Scope.Caller = appCaller("background")
	}
	if sessionID != "" {
		for _, previous := range d.Tasks {
			if previous.Info.SessionID == sessionID && previous.Info.CompletedAt != nil && len(previous.Messages) > len(record.Messages) {
				record.Messages = clone(previous.Messages)
			}
		}
	}
	d.Tasks[scope.RunID] = record
	d.Runs[scope.RunID] = &runRecord{Scope: scope}
	return wire.AgentRunResponse{Run: clone(record.Info), Created: true}, nil
}

func (h *Host) startTask(w http.ResponseWriter, r *http.Request) {
	var req wire.StartAgentRequest
	if err := strictJSON(r.Body, &req); err != nil {
		fail(w, err)
		return
	}
	var out wire.AgentRunResponse
	err := h.config.State.update(r.Context(), func(d *stateData) error {
		var err error
		out, err = h.enqueue(d, r.PathValue("definition"), req, "", "")
		return err
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, out)
}

func (h *Host) continueTask(w http.ResponseWriter, r *http.Request) {
	var req wire.ContinueAgentRequest
	if err := strictJSON(r.Body, &req); err != nil {
		fail(w, err)
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		fail(w, errors.New("localruntime: continuation prompt is required"))
		return
	}
	var out wire.AgentRunResponse
	err := h.config.State.update(r.Context(), func(d *stateData) error {
		exists := false
		for _, task := range d.Tasks {
			if task.Info.SessionID == r.PathValue("session") && task.Info.Definition == r.PathValue("definition") && task.Info.ContractHash == req.ContractHash {
				exists = true
			}
		}
		if !exists {
			return errors.New("localruntime: session is not owned by the definition")
		}
		raw, _ := json.Marshal(req.Prompt)
		var err error
		out, err = h.enqueue(d, r.PathValue("definition"), wire.StartAgentRequest{RequestID: req.RequestID, ContractHash: req.ContractHash, Input: raw}, r.PathValue("session"), "")
		return err
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, out)
}

func (h *Host) getTask(w http.ResponseWriter, r *http.Request) {
	var info wire.AgentRunInfo
	err := h.config.State.update(r.Context(), func(d *stateData) error {
		task := d.Tasks[r.PathValue("id")]
		if task == nil || task.Info.Definition != r.PathValue("definition") {
			return errors.New("localruntime: task not found")
		}
		info = clone(task.Info)
		return nil
	})
	if err != nil {
		http.Error(w, err.Error(), 404)
		return
	}
	writeJSON(w, wire.AgentRunResponse{Run: info})
}

func (h *Host) listTasks(w http.ResponseWriter, r *http.Request) {
	out := wire.ListAgentRunsResponse{Runs: []wire.AgentRunInfo{}}
	err := h.config.State.update(r.Context(), func(d *stateData) error {
		for _, task := range d.Tasks {
			if task.Info.Definition == r.PathValue("definition") && (r.URL.Query().Get("contractHash") == "" || task.Info.ContractHash == r.URL.Query().Get("contractHash")) && (r.URL.Query().Get("sessionId") == "" || task.Info.SessionID == r.URL.Query().Get("sessionId")) && (r.URL.Query().Get("status") == "" || task.Info.Status == r.URL.Query().Get("status")) {
				out.Runs = append(out.Runs, clone(task.Info))
			}
		}
		return nil
	})
	if err != nil {
		fail(w, err)
		return
	}
	sort.Slice(out.Runs, func(i, j int) bool { return out.Runs[i].CreatedAt.After(out.Runs[j].CreatedAt) })
	limit := 100
	if value := r.URL.Query().Get("limit"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 || parsed > 1000 {
			http.Error(w, "localruntime: limit must be 1-1000", 400)
			return
		}
		limit = parsed
	}
	if cursor := r.URL.Query().Get("cursor"); cursor != "" {
		found := false
		for i, run := range out.Runs {
			if run.ID == cursor {
				out.Runs = out.Runs[i+1:]
				found = true
				break
			}
		}
		if !found {
			http.Error(w, "localruntime: cursor does not match task selection", 400)
			return
		}
	}
	if len(out.Runs) > limit {
		out.NextCursor = out.Runs[limit-1].ID
		out.Runs = out.Runs[:limit]
	}
	writeJSON(w, out)
}

func (h *Host) cancelTask(w http.ResponseWriter, r *http.Request) {
	var info wire.AgentRunInfo
	err := h.config.State.update(r.Context(), func(d *stateData) error {
		task := d.Tasks[r.PathValue("id")]
		if task == nil || task.Info.Definition != r.PathValue("definition") {
			return errors.New("localruntime: task not found")
		}
		cancelRecord(d, task)
		info = clone(task.Info)
		return nil
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, wire.AgentRunResponse{Run: info})
}

func cancelRecord(d *stateData, task *taskRecord) {
	if task.Info.CompletedAt != nil {
		return
	}
	now := time.Now().UTC()
	task.Info.Status = "cancelled"
	task.Info.CompletedAt = &now
	task.Info.UpdatedAt = now
	d.Runs[task.Info.ID].Closed = true
	for _, child := range d.Tasks {
		if child.ParentID == task.Info.ID {
			cancelRecord(d, child)
		}
	}
}

// Run owns bounded worker goroutines. Filesystem leases and owner tokens are
// durable; another process can recover expired work with the same task ID.
func (h *Host) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		worked, err := h.WorkOnce(ctx)
		if err != nil && ctx.Err() == nil {
			return err
		}
		if !worked {
			select {
			case <-ctx.Done():
			case <-time.After(50 * time.Millisecond):
			}
		}
	}
	return nil
}

func (h *Host) WorkOnce(ctx context.Context) (bool, error) {
	var chosen *taskRecord
	var def wire.AgentDefinition
	var manifest wire.AgentManifest
	err := h.config.State.update(ctx, func(d *stateData) error {
		if err := h.scheduleCrons(d); err != nil {
			return err
		}
		now := time.Now().UTC()
		ids := make([]string, 0, len(d.Tasks))
		for id := range d.Tasks {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return d.Tasks[ids[i]].Info.CreatedAt.Before(d.Tasks[ids[j]].Info.CreatedAt) })
		for _, id := range ids {
			task := d.Tasks[id]
			if task.Info.CompletedAt != nil || task.Info.Status == "running" && task.LeaseUntil.After(now) {
				continue
			}
			if def, err := definition(d, task.Info.Definition, task.Info.ContractHash); err == nil {
				expired := def.Budget.TimeoutMS > 0 && !now.Before(task.Info.CreatedAt.Add(time.Duration(def.Budget.TimeoutMS)*time.Millisecond))
				if task.ParentID != "" {
					root := d.Tasks[task.ParentID]
					rootDef, err := definition(d, root.Info.Definition, root.Info.ContractHash)
					if err == nil && rootDef.Budget.TimeoutMS > 0 && !now.Before(root.Info.CreatedAt.Add(time.Duration(rootDef.Budget.TimeoutMS)*time.Millisecond)) {
						expired = true
					}
				}
				if expired {
					task.Info.Status = "budget_exceeded"
					task.Info.Error = "localruntime: task deadline exceeded"
					task.Info.CompletedAt = &now
					task.Info.UpdatedAt = now
					d.Runs[id].Closed = true
					for _, child := range d.Tasks {
						if child.ParentID == id {
							cancelRecord(d, child)
						}
					}
					continue
				}
			}
			if cp := task.Checkpoint; cp != nil && cp.Phase == agentruntime.PhaseCompleted && cp.ContractHash == task.Info.ContractHash && cp.Reply != nil {
				task.Info.Status = "completed"
				task.Info.Reply = clone(cp.Reply)
				task.Info.CompletedAt = &now
				task.Info.UpdatedAt = now
				d.Runs[id].Closed = true
				continue
			}
			if task.Info.Status == "waiting" {
				complete := 0
				for _, childID := range task.WaitRequest.IDs {
					child := d.Tasks[childID]
					if child != nil && child.Info.CompletedAt != nil {
						complete++
					}
				}
				ready := complete == len(task.WaitRequest.IDs) || task.WaitRequest.Mode == "any" && complete > 0 || task.WaitDeadline != nil && !now.Before(*task.WaitDeadline)
				if !ready {
					continue
				}
			}
			current, err := definition(d, task.Info.Definition, task.Info.ContractHash)
			if err != nil {
				task.Info.Status = "failed"
				task.Info.Error = err.Error()
				task.Info.CompletedAt = &now
				d.Runs[id].Closed = true
				continue
			}
			if task.Info.Status == "running" && task.Attempts >= current.MaxAttempts {
				task.Info.Status = "failed"
				task.Info.Error = "localruntime: interrupted attempt limit reached"
				task.Info.CompletedAt = &now
				d.Runs[id].Closed = true
				continue
			}
			running := int32(0)
			for _, other := range d.Tasks {
				if other.Info.ID != task.Info.ID && other.Info.Definition == task.Info.Definition && (other.Info.Status == "waiting" || other.Info.Status == "running" && other.LeaseUntil.After(now)) {
					running++
				}
			}
			if running >= current.MaxConcurrency {
				continue
			}
			task.Owner = uuid.NewString()
			fresh := newScope(h.config.AppID, task.Scope.Caller)
			task.Scope.InvocationToken = fresh.InvocationToken
			d.Runs[id].Scope = clone(task.Scope)
			task.LeaseUntil = now.Add(15 * time.Second)
			if task.Info.Status != "waiting" {
				task.Attempts++
			}
			task.Info.Status = "running"
			task.Info.UpdatedAt = now
			if task.Info.StartedAt == nil {
				task.Info.StartedAt = &now
			}
			chosen = clone(task)
			def = current
			manifest = clone(d.Manifest)
			return nil
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	if chosen == nil {
		return h.workJob(ctx)
	}
	err = h.execute(ctx, chosen, def, manifest)
	return true, err
}

func (h *Host) execute(ctx context.Context, task *taskRecord, def wire.AgentDefinition, manifest wire.AgentManifest) error {
	binding, ok := h.config.Models[def.ModelSlot]
	if !ok {
		return fmt.Errorf("localruntime: missing task model slot %q", def.ModelSlot)
	}
	catalog, err := capability.DefinitionCatalog(manifest, *task.Scope.Definition, h.config.Discovery)
	if err != nil {
		return err
	}
	filtered := catalog[:0]
	for _, d := range catalog {
		if d.Path.Kind() == capability.Air && (d.Path.CanonicalOperation() == "output" || d.Path.CanonicalOperation() == "request_upgrade") {
			continue
		}
		filtered = append(filtered, d)
	}
	catalog = filtered
	attempt, cancel := context.WithCancel(ctx)
	defer cancel()
	if def.Budget.TimeoutMS > 0 {
		var timeoutCancel context.CancelFunc
		attempt, timeoutCancel = context.WithDeadline(attempt, task.Info.CreatedAt.Add(time.Duration(def.Budget.TimeoutMS)*time.Millisecond))
		defer timeoutCancel()
	}
	if task.ParentID != "" {
		var deadline time.Time
		if err := h.config.State.update(attempt, func(d *stateData) error {
			root := d.Tasks[task.ParentID]
			rootDef, err := definition(d, root.Info.Definition, root.Info.ContractHash)
			if err != nil {
				return err
			}
			if rootDef.Budget.TimeoutMS > 0 {
				deadline = root.Info.CreatedAt.Add(time.Duration(rootDef.Budget.TimeoutMS) * time.Millisecond)
			}
			return nil
		}); err != nil {
			return err
		}
		if !deadline.IsZero() {
			var stop context.CancelFunc
			attempt, stop = context.WithDeadline(attempt, deadline)
			defer stop()
		}
	}
	done := make(chan struct{})
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-attempt.Done():
				return
			case <-ticker.C:
				err := h.config.State.update(attempt, func(d *stateData) error {
					current := d.Tasks[task.Info.ID]
					if current.Owner != task.Owner || current.Info.CompletedAt != nil {
						return errors.New("localruntime: task lease lost")
					}
					current.LeaseUntil = time.Now().Add(15 * time.Second)
					return nil
				})
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()
	defer func() { close(done); <-joined }()
	controller := &taskController{host: h, id: task.Info.ID, owner: task.Owner, def: def}
	store := &taskStore{controller: controller}
	backend := &AppBackend{URL: h.config.AppURL, Token: h.config.Token, Client: h.config.Client, Scope: task.Scope, Catalog: catalog, Platform: h.config.Platform}
	backend.Platform = &taskPlatform{host: h, app: backend}
	var children []wire.AgentDefinition
	for _, slug := range def.Subagents {
		for _, d := range manifest.AgentDefinitions {
			if d.Slug == slug {
				children = append(children, d)
			}
		}
	}
	notice := ""
	if task.Attempts > 1 {
		notice = "Execution interrupted. In-flight effects have unknown outcomes and must not be blindly retried. JavaScript state is empty."
	}
	result, runErr := agentruntime.Run(attempt, agentruntime.Input{Definition: def, Subagents: children, Message: task.Input, Model: &taskModel{Model: binding.Model, controller: controller}, ModelLimits: binding.Limits, Store: store, Controller: controller, Sink: &discardEvents{}, Backend: backend, Capabilities: catalog, ExecutorFactory: h.config.ExecutorFactory, RecoveryNotice: notice, Redactor: func(s string) string {
		for _, secret := range []string{h.config.Token, task.Scope.InvocationToken} {
			s = strings.ReplaceAll(s, secret, "[REDACTED]")
		}
		return s
	}})
	settleCtx, settleCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer settleCancel()
	return h.config.State.update(settleCtx, func(d *stateData) error {
		current := d.Tasks[task.Info.ID]
		if current.Owner != task.Owner || current.Info.CompletedAt != nil {
			return nil
		}
		now := time.Now().UTC()
		current.Info.UpdatedAt = now
		if ctx.Err() != nil {
			current.LeaseUntil = time.Time{}
			return nil
		}
		if errors.Is(runErr, agentruntime.ErrWaiting) {
			current.Info.Status = "waiting"
			current.LeaseUntil = time.Time{}
			return nil
		}
		current.Info.CompletedAt = &now
		d.Runs[current.Info.ID].Closed = true
		if runErr != nil {
			current.Info.Status = "failed"
			current.Info.Error = runErr.Error()
			if errors.Is(attempt.Err(), context.DeadlineExceeded) || strings.Contains(runErr.Error(), "localruntime: reasoning budget exceeded") || strings.Contains(runErr.Error(), "localruntime: token budget exceeded") {
				current.Info.Status = "budget_exceeded"
			}
		} else if result == nil || result.Reply == nil {
			current.Info.Status = "failed"
			current.Info.Error = "localruntime: task ended without a reply"
		} else {
			current.Info.Status = "completed"
			current.Info.Reply = clone(result.Reply)
		}
		for _, child := range d.Tasks {
			if child.ParentID == current.Info.ID && child.Info.CompletedAt == nil {
				cancelRecord(d, child)
			}
		}
		return nil
	})
}

func equalJSON(a, b json.RawMessage) bool {
	var x, y any
	da := json.NewDecoder(bytes.NewReader(a))
	da.UseNumber()
	db := json.NewDecoder(bytes.NewReader(b))
	db.UseNumber()
	if da.Decode(&x) != nil || db.Decode(&y) != nil {
		return false
	}
	aa, _ := json.Marshal(x)
	bb, _ := json.Marshal(y)
	return bytes.Equal(aa, bb)
}
