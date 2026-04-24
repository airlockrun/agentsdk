package agentsdk

import (
	"context"
	"sync"
	"time"
)

// Rolling "background" run used for model calls made outside any dispatcher-
// bound context (bootstrap code, detached goroutines, etc.). One shared run
// per Agent with an inactivity window; auto-flushed when stale.

const (
	backgroundInactivityWindow = 30 * time.Second
	backgroundMaxDuration      = 5 * time.Minute
	backgroundMaxActions       = 1000
	backgroundFlushInterval    = 10 * time.Second
)

type backgroundState struct {
	mu         sync.Mutex
	run        *run
	createdAt  time.Time
	lastUsedAt time.Time
	stopCh     chan struct{}
	stopOnce   sync.Once
}

// backgroundRunFor returns the current rolling background run, creating or
// rotating it as needed. Called by runForCall when ctx has no run bound.
func (a *Agent) backgroundRunFor(ctx context.Context) *run {
	a.bg.mu.Lock()
	defer a.bg.mu.Unlock()

	now := time.Now()
	if a.bg.run != nil {
		age := now.Sub(a.bg.createdAt)
		stale := now.Sub(a.bg.lastUsedAt) > backgroundInactivityWindow
		overflowed := len(a.bg.run.actions) >= backgroundMaxActions || age >= backgroundMaxDuration

		if stale || overflowed {
			// Flush out of band; no failure path matters — completion is
			// observational, not correctness-critical.
			go a.bg.run.complete(context.Background(), "success", "", "")
			a.bg.run = nil
		}
	}

	if a.bg.run == nil {
		a.bg.run = a.newRunFromAirlock(ctx, "background", "")
		a.bg.createdAt = now
	}
	a.bg.lastUsedAt = now
	return a.bg.run
}

// startBackgroundFlusher runs a ticker that closes a stale background run
// after the inactivity window elapses. Called from Serve().
func (a *Agent) startBackgroundFlusher() {
	a.bg.stopCh = make(chan struct{})
	go func() {
		t := time.NewTicker(backgroundFlushInterval)
		defer t.Stop()
		for {
			select {
			case <-a.bg.stopCh:
				return
			case <-t.C:
				a.bg.mu.Lock()
				if a.bg.run != nil && time.Since(a.bg.lastUsedAt) > backgroundInactivityWindow {
					r := a.bg.run
					a.bg.run = nil
					a.bg.mu.Unlock()
					_ = r.complete(context.Background(), "success", "", "")
					continue
				}
				a.bg.mu.Unlock()
			}
		}
	}()
}

// stopBackgroundFlusher signals the flusher to stop and flushes any open
// background run. Called from Serve()'s shutdown path.
func (a *Agent) stopBackgroundFlusher() {
	a.bg.stopOnce.Do(func() {
		if a.bg.stopCh != nil {
			close(a.bg.stopCh)
		}
	})
	a.bg.mu.Lock()
	r := a.bg.run
	a.bg.run = nil
	a.bg.mu.Unlock()
	if r != nil {
		_ = r.complete(context.Background(), "success", "", "")
	}
}

// runForCall is the one resolver used by every ctx-aware Agent method.
// Precedence: dispatcher-bound run → route-lazy run → Agent background run.
func (a *Agent) runForCall(ctx context.Context) *run {
	if r := runFromContext(ctx); r != nil {
		return r
	}
	if l := lazyRunFromContext(ctx); l != nil {
		return l.get(ctx)
	}
	return a.backgroundRunFor(ctx)
}

// newRunFromAirlock asks Airlock for a fresh run ID and returns a run struct.
// Used by both lazyRun (trigger_type="code") and backgroundRunFor
// (trigger_type="background").
func (a *Agent) newRunFromAirlock(ctx context.Context, triggerType, triggerRef string) *run {
	var resp CreateRunResponse
	req := CreateRunRequest{TriggerType: triggerType, TriggerRef: triggerRef}
	if err := a.client.doJSON(ctx, "POST", "/api/agent/run/create", req, &resp); err != nil {
		// Fail loud: background/lazy runs are observability; if we can't
		// create one, returning a disconnected run would silently lose
		// audits. Prefer a panic the operator sees.
		panic("agentsdk: /api/agent/run/create failed: " + err.Error())
	}
	return newRun(a, resp.RunID, "", "", ctx)
}
