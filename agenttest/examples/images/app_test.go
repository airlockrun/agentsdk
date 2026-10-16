package imagesexample

import (
	"encoding/base64"
	"github.com/airlockrun/agentsdk/wire"
	"testing"
)

func TestImageDefinitionAndPackagedFixture(t *testing.T) {
	manifest := NewApp().Manifest()
	if err := wire.ValidateAgentDefinitions(manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.AgentDefinitions) != 1 || len(manifest.StartupHooks) != 1 {
		t.Fatalf("image task declarations: definitions=%d startup=%d", len(manifest.AgentDefinitions), len(manifest.StartupHooks))
	}
	found := false
	for _, directory := range manifest.Directories {
		if directory.Path == "evidence" || directory.Path == "/evidence" {
			found = true
			if directory.Read != wire.AccessInternal {
				t.Fatal("image fixture leaked human read access")
			}
		}
	}
	if !found {
		t.Fatal("missing evidence namespace")
	}
	data, err := base64.StdEncoding.DecodeString(probeGIF)
	if err != nil || len(data) < 6 || string(data[:6]) != "GIF89a" {
		t.Fatal("invalid source-owned fixture")
	}
}
