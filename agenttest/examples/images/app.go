package imagesexample

import (
	"context"
	"encoding/base64"
	"github.com/airlockrun/agentsdk"
	"strings"
	"time"
)

type Output struct {
	Description string `json:"description"`
}

const probeGIF = "R0lGODlhAQABAIAAAAAAAP///ywAAAAAAQABAAACAUwAOw=="

// NewApp declares a real-host image task and seeds its tiny source-owned fixture
// through the real app storage API at startup. No host catalogue is simulated.
func NewApp() *agentsdk.Agent {
	a := agentsdk.New(agentsdk.Config{Description: "Stored image example"})
	a.RegisterDirectory("evidence", agentsdk.DirectoryOpts{Read: agentsdk.AccessInternal, Write: agentsdk.AccessInternal, List: agentsdk.AccessInternal, Description: "Task image evidence"})
	a.RegisterModel(&agentsdk.ModelSlot{Slug: "image_probe", Capability: agentsdk.CapText, Description: "Inspect stored image using a vision-capable text model"})
	a.OnStart("seed source fixture", func(ctx context.Context) error {
		_, err := a.WriteFile(ctx, "evidence/probe.gif", base64.NewDecoder(base64.StdEncoding, strings.NewReader(probeGIF)), "image/gif")
		return err
	})
	agentsdk.RegisterAgent(a, &agentsdk.AgentDefinition[struct{}, Output]{Slug: "image_probe", ModelSlot: "image_probe", Description: "Describe a stored image", Instructions: "Attach evidence/probe.gif using air.attachToContext, inspect its image, then submit kind output with a description. Do not use unrelated capabilities.", Capabilities: &agentsdk.AgentCapabilityPolicy{Allow: []agentsdk.AgentCapability{agentsdk.CapabilityAttachToContext, agentsdk.CapabilityFileStat, agentsdk.CapabilityFileRead}}, Budget: &agentsdk.AgentBudget{Steps: 4, Timeout: time.Minute, Tokens: 30000}, MaxAttempts: 1, MaxConcurrency: 1})
	return a
}
