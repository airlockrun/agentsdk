package agentsdk

import (
	"context"
	"sync"
)

// lazyRun is stashed in ctx by the route dispatcher. A run is materialized
// (POST /api/agent/run/create, with trigger_type="code") only when the first
// call inside the route handler asks for one. Handlers that make zero model
// calls / zero log entries pay zero cost.
type lazyRun struct {
	mu         sync.Mutex
	run        *run
	agent      *Agent
	triggerRef string

	// Scope keys threaded in by the dispatching handler. CheckFileAccess
	// consults these for tool bodies that read from scoped directories
	// without materializing a run (run isn't created until something
	// actually logs / calls the model). When the run is later
	// materialized, these are copied into it.
	parentRunID    string
	conversationID string
	userID         string
}

func (l *lazyRun) get(ctx context.Context) *run {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.run == nil {
		l.run = l.agent.newRunFromAirlock(ctx, "code", l.triggerRef)
		// Carry the scope keys threaded in at dispatch time onto the
		// freshly-materialized run so CheckFileAccess matches them
		// consistently regardless of whether the run was lazy or eager.
		l.run.parentRunID = l.parentRunID
		l.run.conversationID = l.conversationID
		l.run.userID = l.userID
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
