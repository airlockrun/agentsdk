package connectortest_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/airlockrun/agentsdk/connector"
	"github.com/airlockrun/agentsdk/connector/connectortest"
)

func TestNewInvokesDefinitionFactoryWithoutRuntimeState(t *testing.T) {
	stateDirectory := filepath.Join(t.TempDir(), "state")
	env := connectortest.New(t, func() *connector.Runtime {
		if _, err := os.Stat(stateDirectory); !os.IsNotExist(err) {
			t.Fatalf("state exists during definition: %v", err)
		}
		return connector.New(connector.Config{
			Kind: "factory", Contract: connector.DefineContract("io.airlockrun.factory_test"), Name: "Factory", Description: "Factory test.", ArtifactVersion: "1",
			Targets: []string{connector.PlatformLinuxAMD64},
		})
	})
	if env.Runtime == nil || env.Manifest.Interface.Kind != "factory" {
		t.Fatalf("environment = %+v", env)
	}
	if _, err := os.Stat(stateDirectory); !os.IsNotExist(err) {
		t.Fatalf("manifest created runtime state: %v", err)
	}
}
