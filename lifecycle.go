package agentsdk

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime/debug"
	"strings"

	"github.com/airlockrun/agentsdk/wire"
	"go.uber.org/zap"
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
	mode, downTo, err := configuredMigrationMode()
	if err != nil {
		return err
	}
	// Migration-only processes use an isolated database and exit before host
	// sync or startup hooks. They do not consume a live configuration snapshot.
	if mode == migrationUp {
		if err := a.loadEnvConfig(); err != nil {
			return err
		}
	}
	a.initializeRuntime(mode, downTo)
	if mode != migrationUp {
		return errors.New("agentsdk: migration-only execution returned without exiting")
	}
	if len(a.uiChangeTopics) > 0 {
		if err := a.initializeUIChanges(ctx); err != nil {
			return fmt.Errorf("agentsdk: initialize UI changes: %w", err)
		}
	}
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
	a.stopUIChanges()
	var err error
	if a.db != nil && a.db.db != nil {
		err = a.db.db.Close()
	}
	a.markClosed()
	return err
}

func (a *Agent) runStartHooks(ctx context.Context) (err error) {
	lazy := &lazyRun{agent: a, callerAccess: AccessAdmin, startup: true,
		caller: callerFromWire(wire.Caller{Kind: "application", Access: wire.AccessAdmin,
			Origin: wire.CallerOrigin{Interface: "application", Execution: "startup"}})}
	ctx = contextWithLazyRun(contextWithJobRun(ctx, nil), lazy)
	defer func() {
		recovered := recover()
		if r := lazy.materialized(); r != nil {
			status, message, kind, trace := "success", "", "", ""
			if err != nil {
				status, message, kind = "error", err.Error(), wire.ErrorKindAgent
			}
			if recovered != nil {
				status, message, kind, trace = "error", fmt.Sprint(recovered), wire.ErrorKindAgent, string(debug.Stack())
			}
			completionErr := r.complete(ctx, status, message, kind, trace)
			if recovered != nil && completionErr != nil {
				agentLogger().Error("record startup completion", zap.Error(completionErr))
			}
			err = errors.Join(err, completionErr)
		}
		if recovered != nil {
			panic(recovered)
		}
	}()
	for _, hook := range a.startHooks {
		if err := hook.run(ctx); err != nil {
			return fmt.Errorf("agentsdk: startup hook %q: %w", hook.name, err)
		}
	}
	return nil
}
