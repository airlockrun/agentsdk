package agentsdk

import (
	"context"
	"github.com/airlockrun/agentsdk/wire"
	"sync"
)

// lazyRun materializes the exact host-admitted request for host callbacks.
// Caller reads and handlers without host callbacks perform no I/O.
type lazyRun struct {
	mu         sync.Mutex
	run        *run
	agent      *Agent
	proof      string
	startup    bool
	loggingOff bool

	// Dispatch metadata is available without materializing an app-owned run.
	conversationID string
	userID         string
	callerAccess   Access
	caller         Caller
}

func (l *lazyRun) get(ctx context.Context) *run {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.run == nil {
		if l.startup {
			if l.caller.Kind() != CallerApplication {
				panic("agentsdk: startup requires application caller")
			}
			l.run = l.agent.newRunFromAirlock(ctx, "code", "startup")
			return l.run
		}
		if l.proof == "" {
			panic("agentsdk: route dispatch proof is required")
		}
		var response wire.MaterializeRouteResponse
		if err := l.agent.client.doJSON(ctx, "POST", "/api/agent/run/materialize-route", wire.MaterializeRouteRequest{Proof: l.proof}, &response); err != nil {
			panic("agentsdk: materialize route: " + err.Error())
		}
		if response.Runtime.RunID == "" || response.Runtime.InvocationToken == "" || (response.Logging != wire.LoggingOff && response.Logging != wire.LoggingNormal) || response.Runtime.Caller.Kind == "application" {
			panic("agentsdk: invalid host route materialization response")
		}
		l.run = l.agent.borrowRuntimeRun(ctx, response.Runtime)
		l.run.loggingOff = response.Logging == wire.LoggingOff
	}
	return l.run
}

// materialized reports whether get() has ever been called. The dispatcher
// uses this to decide whether to call complete() on handler return.
func (l *lazyRun) materialized() *run {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.run
}
