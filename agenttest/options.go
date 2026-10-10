package agenttest

import (
	"flag"
	"fmt"
	"strings"

	"github.com/airlockrun/agentsdk/capability"
	"github.com/airlockrun/agentsdk/chatruntime"
	"github.com/airlockrun/agentsdk/localruntime"

	"github.com/airlockrun/goai/testutil"
	"github.com/airlockrun/sol/session"
)

// Options configures per-environment models. Zero options select an independent
// configurable mock for every declared text/vision slot and the default text slot.
type Options struct {
	// ExecutorFactory supplies an isolated executor for durable task JavaScript.
	// Omitted factories use the explicit local agentsdk-jsexecutor:local image.
	ExecutorFactory chatruntime.ExecutorFactory
	Resources       localruntime.Resources
	NoAuthResources localruntime.Resources
	EnvVars         map[string]string
	ResourceSession string
	Discovery       capability.Discovery
	Backend         chatruntime.Backend
	// Storage selects a seeded isolated store. Omission creates an empty store.
	// The shared host and model attachment resolver use that same store.
	Storage *FileStorage
	// Models selects live slug/provider/model references by slot. "" is the unnamed
	// default text slot. Unselected slots remain mocked.
	Models map[string]string
	// Mocks configures responses before startup. Live slots retain these mocks
	// without dispatching to them. Omitted ID uses the environment's slot identity.
	Mocks map[string]testutil.MockConfig
	// Limits overrides test-only mock compaction limits. Live limits come from
	// provider resolution. Defaults are Context: 100000, Output: 4096.
	Limits map[string]session.ModelLimits
}

// RegisterFlags explicitly registers agenttest.model on fs.
// Call once during test package setup, before parsing. It performs no model,
// environment, credential or network lookup. Pass the returned options to
// NewWithOptions after parsing; New does not read package flags.
func RegisterFlags(fs *flag.FlagSet) *Options {
	if fs == nil {
		panic("agenttest: RegisterFlags requires a FlagSet")
	}
	options := &Options{}
	fs.Var(modelSelections{options}, "agenttest.model", "live slot=slug/provider/model (repeatable); @default selects the unnamed text model")
	return options
}

type modelSelections struct{ options *Options }

func (v modelSelections) String() string {
	var selections []string
	for _, slug := range sortedModelKeys(v.options.Models) {
		label := slug
		if label == "" {
			label = "@default"
		}
		selections = append(selections, label+"="+v.options.Models[slug])
	}
	return strings.Join(selections, ",")
}
func (v modelSelections) Set(value string) error {
	slot, model, ok := strings.Cut(value, "=")
	if !ok || slot == "" || strings.ContainsAny(slot, " \t\r\n") {
		return fmt.Errorf("agenttest: model selection %q must be slot=provider/model", value)
	}
	if err := checkModelName(model); err != nil {
		return err
	}
	if slot == "@default" {
		slot = ""
	}
	if _, exists := v.options.Models[slot]; exists {
		return fmt.Errorf("agenttest: duplicate model selection for slot %q", slot)
	}
	if v.options.Models == nil {
		v.options.Models = make(map[string]string)
	}
	v.options.Models[slot] = model
	return nil
}
