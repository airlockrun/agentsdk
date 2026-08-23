package agentsdk

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

type startHook struct {
	name string
	run  func(context.Context) error
}

// OnStart registers process-local initialization that runs once after the
// runtime is connected and synchronized, before the agent becomes ready.
// Startup hooks must only construct disposable local state; durable work
// belongs in a registered job.
func (a *Agent) OnStart(name string, run func(context.Context) error) {
	done := a.beginRegistration("OnStart")
	defer done()
	if strings.TrimSpace(name) == "" {
		panic("agentsdk: OnStart: name is required")
	}
	if run == nil {
		panic("agentsdk: OnStart(" + name + "): callback is required")
	}
	for _, hook := range a.startHooks {
		if hook.name == name {
			panic("agentsdk: duplicate OnStart: " + name)
		}
	}
	a.startHooks = append(a.startHooks, startHook{name: name, run: run})
}

// Start freezes declarations, initializes runtime dependencies, synchronizes
// the manifest with Airlock, and runs process-local startup hooks. It does not
// start the HTTP server. Most applications call Serve, which calls Start.
func (a *Agent) Start(ctx context.Context) (retErr error) {
	if ctx == nil {
		panic("agentsdk: Agent.Start requires a context")
	}
	if mode := os.Getenv("AIRLOCK_AGENT_MODE"); mode != "" {
		return fmt.Errorf("agentsdk: Agent.Start is unavailable when AIRLOCK_AGENT_MODE=%s", mode)
	}
	if err := a.beginStart(); err != nil {
		return err
	}
	defer func() {
		if recovered := recover(); recovered != nil {
			a.cleanupFailedStart()
			panic(recovered)
		}
		if retErr != nil {
			retErr = errors.Join(retErr, a.cleanupFailedStart())
		}
	}()

	a.freeze()
	a.initializeRuntime()
	if err := a.syncWithAirlock(ctx); err != nil {
		return err
	}
	if err := a.runStartHooks(ctx); err != nil {
		return err
	}
	a.finishStart()
	return nil
}

func (a *Agent) cleanupFailedStart() error {
	var err error
	if a.db != nil && a.db.db != nil {
		err = a.db.db.Close()
	}
	a.markClosed()
	return err
}

func (a *Agent) runStartHooks(ctx context.Context) error {
	for _, hook := range a.startHooks {
		if err := hook.run(ctx); err != nil {
			return fmt.Errorf("agentsdk: startup hook %q: %w", hook.name, err)
		}
	}
	return nil
}
