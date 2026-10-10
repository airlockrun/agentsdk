package localruntime

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/airlockrun/agentsdk/agentruntime"
	"github.com/airlockrun/agentsdk/wire"
	"github.com/google/uuid"
)

func jobDefinition(d *stateData, name string, version int32, inputHash, outputHash string) (wire.JobHandlerDef, error) {
	for _, def := range d.Manifest.JobHandlers {
		if def.Name == name && def.Version == version && def.InputSchemaHash == inputHash && def.OutputSchemaHash == outputHash {
			return def, nil
		}
	}
	return wire.JobHandlerDef{}, errors.New("localruntime: job handler contract is not registered")
}

func (h *Host) enqueueJob(w http.ResponseWriter, r *http.Request) {
	var req wire.EnqueueJobRequest
	if err := strictJSON(r.Body, &req); err != nil {
		fail(w, err)
		return
	}
	id, err := uuid.Parse(req.ID)
	if err != nil || id == uuid.Nil || id.String() != req.ID {
		fail(w, errors.New("localruntime: canonical job UUID is required"))
		return
	}
	var out wire.EnqueueJobResponse
	err = h.config.State.update(r.Context(), func(d *stateData) error {
		def, err := jobDefinition(d, req.Name, req.Version, req.InputSchemaHash, req.OutputSchemaHash)
		if err != nil {
			return err
		}
		if err := agentruntime.ValidateJSON(def.InputSchema, req.Input); err != nil {
			return err
		}
		if previous := d.Jobs[req.ID]; previous != nil {
			if previous.Info.HandlerName != req.Name || previous.Info.HandlerVersion != req.Version || !equalJSON(previous.Info.Input, req.Input) || previous.Info.InputSchemaHash != req.InputSchemaHash || previous.Info.OutputSchemaHash != req.OutputSchemaHash || !sameTime(previous.Info.ScheduledAt, req.ScheduledAt) {
				return errors.New("localruntime: job ID payload conflict")
			}
			out.Job = clone(previous.Info)
			return nil
		}
		run := d.Runs[r.Header.Get("X-Airlock-Run-ID")]
		if run == nil || run.Closed {
			return errors.New("localruntime: source run is closed")
		}
		now := time.Now().UTC()
		caller := clone(run.Scope.Caller)
		caller.Origin.Execution = "job"
		if err := caller.Validate(); err != nil {
			return err
		}
		info := wire.JobInfo{ID: req.ID, AgentID: h.config.AppID, HandlerName: req.Name, HandlerVersion: req.Version, InputSchemaHash: req.InputSchemaHash, OutputSchemaHash: req.OutputSchemaHash, Status: "queued", Input: req.Input, MaxAttempts: def.MaxAttempts, AttemptLimit: def.MaxAttempts, SourceRunID: run.Scope.RunID, ScheduledAt: req.ScheduledAt, CreatedAt: now, UpdatedAt: now}
		d.Jobs[req.ID] = &jobRecord{Info: info, Caller: caller, ResourceSession: run.ResourceSession}
		out = wire.EnqueueJobResponse{Job: info, Created: true}
		return nil
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, out)
}
func sameTime(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

func (h *Host) getJob(w http.ResponseWriter, r *http.Request) {
	var out wire.GetJobResponse
	err := h.config.State.update(r.Context(), func(d *stateData) error {
		job := d.Jobs[r.PathValue("id")]
		if job == nil {
			return errors.New("localruntime: job not found")
		}
		out.Job = clone(job.Info)
		return nil
	})
	if err != nil {
		http.Error(w, err.Error(), 404)
		return
	}
	writeJSON(w, out)
}
func (h *Host) cancelJob(w http.ResponseWriter, r *http.Request) {
	err := h.config.State.update(r.Context(), func(d *stateData) error {
		job := d.Jobs[r.PathValue("id")]
		if job == nil {
			return errors.New("localruntime: job not found")
		}
		if job.Info.CompletedAt == nil {
			now := time.Now().UTC()
			job.Info.Status = "cancelled"
			job.Info.CompletedAt = &now
			job.Info.UpdatedAt = now
			if job.RunID != "" {
				d.Runs[job.RunID].Closed = true
			}
		}
		return nil
	})
	if err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(204)
}
func (h *Host) progressJob(w http.ResponseWriter, r *http.Request) {
	var req wire.UpdateJobProgressRequest
	if err := strictJSON(r.Body, &req); err != nil {
		fail(w, err)
		return
	}
	err := h.config.State.update(r.Context(), func(d *stateData) error {
		job := d.Jobs[r.PathValue("id")]
		token := r.Header.Get("X-Airlock-Job-Lease-Token")
		if job == nil || job.Info.Status != "running" || job.Info.AttemptCount != req.Attempt || !job.LeaseUntil.After(time.Now()) || token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(job.Owner)) != 1 {
			return errors.New("localruntime: stale job attempt")
		}
		job.Info.Progress = &wire.JobProgress{Phase: req.Phase, Message: req.Message, Completed: req.Completed, Total: req.Total}
		job.Info.UpdatedAt = time.Now().UTC()
		return nil
	})
	if err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(204)
}

func (h *Host) workJob(ctx context.Context) (bool, error) {
	var chosen *jobRecord
	var def wire.JobHandlerDef
	var scope wire.RuntimeContext
	err := h.config.State.update(ctx, func(d *stateData) error {
		now := time.Now().UTC()
		ids := make([]string, 0, len(d.Jobs))
		for id := range d.Jobs {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return d.Jobs[ids[i]].Info.CreatedAt.Before(d.Jobs[ids[j]].Info.CreatedAt) })
		for _, id := range ids {
			job := d.Jobs[id]
			if job.Info.CompletedAt != nil || job.Info.ScheduledAt != nil && job.Info.ScheduledAt.After(now) || job.Info.Status == "running" && job.LeaseUntil.After(now) {
				continue
			}
			if job.Info.Status == "running" {
				job.Info.Status = "failed"
				job.Info.LastError = "Execution interrupted: job effects have unknown outcomes; this job is not automatically replayed."
				job.Info.CompletedAt = &now
				d.Runs[job.RunID].Closed = true
				continue
			}
			current, e := jobDefinition(d, job.Info.HandlerName, job.Info.HandlerVersion, job.Info.InputSchemaHash, job.Info.OutputSchemaHash)
			if e != nil {
				return e
			}
			active := int32(0)
			for _, other := range d.Jobs {
				if other.Info.HandlerName == job.Info.HandlerName && other.Info.Status == "running" && other.LeaseUntil.After(now) {
					active++
				}
			}
			if active >= current.MaxConcurrency {
				continue
			}
			job.Owner = uuid.NewString()
			job.LeaseUntil = now.Add(15 * time.Second)
			job.Info.AttemptCount++
			job.Info.Status = "running"
			job.Info.StartedAt = &now
			job.Info.UpdatedAt = now
			scope = newScope(h.config.AppID, job.Caller)
			job.RunID = scope.RunID
			scope.Job = &wire.RuntimeJobContext{ID: id, Attempt: int(job.Info.AttemptCount), LeaseToken: job.Owner}
			d.Runs[scope.RunID] = &runRecord{Scope: scope, ResourceSession: job.ResourceSession}
			chosen = clone(job)
			def = current
			return nil
		}
		return nil
	})
	if err != nil || chosen == nil {
		return false, err
	}
	attempt, cancel := context.WithTimeout(ctx, time.Duration(def.TimeoutMs)*time.Millisecond)
	defer cancel()
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
					j := d.Jobs[chosen.Info.ID]
					if j.Owner != chosen.Owner || j.Info.CompletedAt != nil {
						return errors.New("localruntime: job lease lost")
					}
					j.LeaseUntil = time.Now().Add(15 * time.Second)
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
	payload, _ := json.Marshal(wire.JobRunRequest{ID: chosen.Info.ID, Name: def.Name, Version: def.Version, InputSchemaHash: def.InputSchemaHash, OutputSchemaHash: def.OutputSchemaHash, Attempt: chosen.Info.AttemptCount, TimeoutMs: def.TimeoutMs, Input: chosen.Info.Input, ScheduledAt: chosen.Info.ScheduledAt, Caller: chosen.Caller})
	req, err := http.NewRequestWithContext(attempt, "POST", fmt.Sprintf("%s/job/%s/%d", h.config.AppURL, def.Name, def.Version), bytes.NewReader(payload))
	if err != nil {
		return true, err
	}
	req.Header.Set("Authorization", "Bearer "+h.config.Token)
	req.Header.Set("X-Run-ID", scope.RunID)
	req.Header.Set(wire.InvocationTokenHeader, scope.InvocationToken)
	req.Header.Set("X-Airlock-Job-Lease-Token", chosen.Owner)
	var output wire.JobRunResponse
	resp, callErr := h.config.Client.Do(req)
	if callErr == nil {
		if resp.StatusCode != http.StatusOK {
			callErr = fmt.Errorf("localruntime: job delivery HTTP %d", resp.StatusCode)
		} else {
			callErr = strictJSON(resp.Body, &output)
		}
		resp.Body.Close()
	}
	settle, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	err = h.config.State.update(settle, func(d *stateData) error {
		job := d.Jobs[chosen.Info.ID]
		if job.Owner != chosen.Owner || job.Info.CompletedAt != nil {
			return nil
		}
		now := time.Now().UTC()
		job.Info.UpdatedAt = now
		if ctx.Err() != nil {
			job.LeaseUntil = time.Time{}
			return nil
		}
		if callErr == nil && output.Status == "retry" && job.Info.AttemptCount < job.Info.MaxAttempts {
			job.Info.Status = "queued"
			job.Info.LastError = output.Error
			d.Runs[scope.RunID].Closed = true
			return nil
		}
		job.Info.CompletedAt = &now
		d.Runs[scope.RunID].Closed = true
		if callErr != nil {
			job.Info.Status = "failed"
			job.Info.LastError = callErr.Error()
		} else if output.Status == "success" {
			if err := agentruntime.ValidateJSON(def.OutputSchema, output.Output); err != nil {
				job.Info.Status = "failed"
				job.Info.LastError = err.Error()
			} else {
				job.Info.Status = "succeeded"
				job.Info.Output = output.Output
			}
		} else {
			job.Info.Status = "failed"
			job.Info.LastError = output.Error
		}
		return nil
	})
	return true, err
}
