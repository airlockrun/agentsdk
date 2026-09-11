package agenttest

import (
	"context"
	"errors"
	"io"

	"github.com/airlockrun/agentsdk/capability"
	"github.com/airlockrun/agentsdk/chatruntime"
	"github.com/airlockrun/agentsdk/jsexec"
)

// ExecutorConfig selects exactly one transport. OpenTransport lets a builder
// inject a socket-free transport to an isolated executor owned by its host;
// test containers must not mount the Docker socket. Image selects local Docker.
// Both transports execute the same framed jsexec protocol and Deno runtime.
type ExecutorConfig struct {
	Image         string
	OpenTransport func(context.Context) (io.ReadWriteCloser, error)
	Limits        jsexec.Limits
	// User is the optional caller display context exposed to JavaScript. Use
	// the same identity as Chat's RuntimeContext; it does not authorize calls.
	User *jsexec.User
}

// Executor returns a lazy factory. It does not connect to Docker or open a
// transport until an approved run_js call needs an executor. Build local images
// explicitly with jsexec.BuildImage before running tests that execute scripts.
func Executor(config ExecutorConfig) (chatruntime.ExecutorFactory, error) {
	if (config.Image == "") == (config.OpenTransport == nil) {
		return nil, errors.New("agenttest: exactly one of Image or OpenTransport is required")
	}
	if config.Limits == (jsexec.Limits{}) {
		return nil, errors.New("agenttest: explicit executor Limits are required")
	}
	return func(ctx context.Context, catalog []capability.Definition) (jsexec.Session, error) {
		options := jsexec.Options{Limits: config.Limits, Bindings: make([]jsexec.Binding, 0, len(catalog)), User: config.User}
		for _, d := range catalog {
			if d.Target == capability.Executor {
				continue
			}
			options.Bindings = append(options.Bindings, jsexec.Binding{Name: d.Path.ID(), Path: d.Path.JSParts()})
		}
		if config.OpenTransport == nil {
			return jsexec.NewDockerSession(ctx, config.Image, options)
		}
		transport, err := config.OpenTransport(ctx)
		if err != nil {
			return nil, err
		}
		client, err := jsexec.NewClient(transport, options)
		if err != nil && transport != nil {
			_ = transport.Close()
		}
		return client, err
	}, nil
}
