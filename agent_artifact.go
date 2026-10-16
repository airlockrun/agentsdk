package agentsdk

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

// OpenAcceptedArtifact streams the immutable bytes pinned in a completed reply.
// The artifact must exactly match this handle's persisted run manifest.
func (h *AgentHandle[In, Out]) OpenAcceptedArtifact(ctx context.Context, id string, artifact AgentArtifact) (io.ReadCloser, error) {
	run, err := h.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if run.Status != AgentRunCompleted || run.Reply == nil || run.Reply.Kind != AgentReplyOutput {
		return nil, errors.New("agentsdk: accepted output required")
	}
	index := -1
	for i, a := range run.Reply.Artifacts {
		if a == artifact {
			index = i
			break
		}
	}
	if index < 0 {
		return nil, errors.New("agentsdk: artifact is not in the accepted run manifest")
	}
	path := "/api/agent/agents/" + url.PathEscape(h.Slug()) + "/runs/" + url.PathEscape(id) + "/artifacts/" + strconv.Itoa(index) + "?contractHash=" + url.QueryEscape(run.ContractHash)
	response, err := h.agent.client.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		defer response.Body.Close()
		return nil, fmt.Errorf("agentsdk: open accepted artifact: status %d", response.StatusCode)
	}
	return response.Body, nil
}
