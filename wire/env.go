package wire

// EnvConfigProtocol identifies immutable process-start configuration.
const EnvConfigProtocol = "airlock.env-config.v1"

const EnvConfigVariable = "AIRLOCK_ENV_CONFIG"

// EnvConfigSnapshot is private host-to-runtime startup data. Values include
// configured slots only; application declarations supply defaults for unset slots.
// It must never be logged or included in operator metadata.
type EnvConfigSnapshot struct {
	Protocol string            `json:"protocol"`
	Revision int64             `json:"revision"`
	Attempt  string            `json:"attempt"`
	Values   map[string]string `json:"values"`
}
