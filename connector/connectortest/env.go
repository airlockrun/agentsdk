// Package connectortest provides factory-first connector integration helpers.
package connectortest

import (
	"testing"

	"github.com/airlockrun/agentsdk/connector"
	"github.com/airlockrun/agentsdk/connector/protocol"
)

// Env contains a connector definition and its validated offline manifest.
type Env struct {
	Runtime  *connector.Runtime
	Manifest protocol.Manifest
}

// New invokes factory with connector runtime selection cleared, then builds
// the same frozen manifest used by runtime execution. Settings and other
// runtime values remain unavailable while factory executes.
func New(t *testing.T, factory func() *connector.Runtime) *Env {
	t.Helper()
	if factory == nil {
		t.Fatal("connectortest: factory is required")
	}
	t.Setenv("AIRLOCK_CONNECTOR_MODE", "")
	t.Setenv("AIRLOCK_CONNECTOR_INSTALLATION_ID", "")
	runtime := factory()
	if runtime == nil {
		t.Fatal("connectortest: factory returned nil")
	}
	return &Env{Runtime: runtime, Manifest: runtime.Manifest()}
}
