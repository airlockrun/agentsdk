package agentsdk

import "context"

// TopicHandle is a compile-time binding to a registered topic.
// Returned by Agent.RegisterTopic; used for type-safe publishing.
type TopicHandle struct {
	slug  string
	agent *Agent
}

// Publish sends display parts to all conversations subscribed to this topic.
func (h *TopicHandle) Publish(ctx context.Context, parts []DisplayPart) error {
	for i := range parts {
		ResolveDisplayPart(&parts[i])
	}
	req := PrintRequest{Parts: parts, Topic: h.slug}
	return h.agent.client.doJSON(ctx, "POST", "/api/agent/print", req, nil)
}
