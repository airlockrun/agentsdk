package connector

import (
	"context"
	"strings"
)

type startHook struct {
	name string
	run  func(context.Context) error
}

// OnStart registers process-local initialization for the run command. Hooks
// execute in registration order after settings and directory roots are bound.
func (r *Runtime) OnStart(name string, run func(context.Context) error) {
	if r == nil {
		panic("connector: OnStart runtime is required")
	}
	r.definitionMu.Lock()
	defer r.definitionMu.Unlock()
	if r.frozen {
		panic("connector: registrations are frozen after Manifest or Run")
	}
	if strings.TrimSpace(name) == "" || run == nil {
		panic("connector: OnStart name and callback are required")
	}
	if r.startHookNames[name] {
		panic("connector: duplicate OnStart: " + name)
	}
	r.startHookNames[name] = true
	r.startHooks = append(r.startHooks, startHook{name: name, run: run})
}

func (r *Runtime) runStartHooks(ctx context.Context) error {
	for _, hook := range r.startHooks {
		if err := hook.run(ctx); err != nil {
			return err
		}
	}
	return nil
}
