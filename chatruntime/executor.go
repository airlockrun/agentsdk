package chatruntime

import (
	"context"
	"errors"
	"io"

	"github.com/airlockrun/agentsdk/capability"
	"github.com/airlockrun/agentsdk/jsexec"
)

// ExecutorFromTransport creates isolated sessions using a host-owned transport.
// Opening is lazy. The host supplies authorized capabilities and owns the actual
// executor process; this factory does not provision containers or runtime state.
func ExecutorFromTransport(open func(context.Context) (io.ReadWriteCloser, error), limits jsexec.Limits, user *jsexec.User) (ExecutorFactory, error) {
	if open == nil || limits == (jsexec.Limits{}) {
		return nil, errors.New("chatruntime: transport opener and explicit executor limits are required")
	}
	var display *jsexec.User
	if user != nil {
		copy := *user
		display = &copy
	}
	return func(ctx context.Context, catalog []capability.Definition) (jsexec.Session, error) {
		options := jsexec.Options{Limits: limits, User: display, Bindings: make([]jsexec.Binding, 0, len(catalog))}
		for _, definition := range catalog {
			if definition.Target != capability.Executor {
				options.Bindings = append(options.Bindings, jsexec.Binding{Name: definition.Path.ID(), Path: definition.Path.JSParts()})
			}
		}
		transport, err := open(ctx)
		if err != nil {
			return nil, err
		}
		if transport == nil {
			return nil, errors.New("chatruntime: executor opener returned nil transport")
		}
		client, err := jsexec.NewClient(transport, options)
		if err != nil {
			return nil, errors.Join(err, transport.Close())
		}
		return client, nil
	}, nil
}
