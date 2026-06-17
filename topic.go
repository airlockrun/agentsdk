package agentsdk

import "context"

// TopicHandle is a compile-time binding to a registered topic.
// Returned by Agent.RegisterTopic; used for type-safe publishing.
type TopicHandle struct {
	slug    string
	perUser bool
	agent   *Agent
}

// Publish sends display parts to all conversations subscribed to this topic.
// It panics on a PerUser topic — those deliver only via PublishToUser, so a
// broadcast would leak one user's content to every subscriber.
func (h *TopicHandle) Publish(ctx context.Context, parts []DisplayPart) error {
	if h.perUser {
		panic("agentsdk: Publish on PerUser topic " + h.slug + ": use PublishToUser")
	}
	return h.publish(ctx, "", parts)
}

// PublishToUser sends display parts only to the given user's conversations
// subscribed to this topic. userID is the internal-user uuid (User.ID).
func (h *TopicHandle) PublishToUser(ctx context.Context, userID string, parts []DisplayPart) error {
	if userID == "" {
		panic("agentsdk: PublishToUser on topic " + h.slug + ": userID is required")
	}
	return h.publish(ctx, userID, parts)
}

func (h *TopicHandle) publish(ctx context.Context, userID string, parts []DisplayPart) error {
	for i := range parts {
		ResolveDisplayPart(&parts[i])
	}
	req := PrintRequest{Parts: parts, Topic: h.slug, UserID: userID}
	return h.agent.client.doJSON(ctx, "POST", "/api/agent/print", req, nil)
}
