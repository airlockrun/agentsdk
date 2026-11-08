package agentsdk

import "github.com/airlockrun/agentsdk/wire"

// Capabilities declares features this application needs. Each platform maps
// supported features to its own permission mechanisms; declarations do not grant
// permission or guarantee platform support.
type Capabilities = wire.AppCapabilities

func (a *Agent) capabilityManifest() *wire.AppCapabilities {
	if a.capabilities == (Capabilities{}) {
		return nil
	}
	c := a.capabilities
	return &c
}
