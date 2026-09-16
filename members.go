package agentsdk

import (
	"context"
	"net/http"
)

// AgentMember is one principal granted access to this agent. A principal can
// be an individual user or an Airlock-managed group. Only a member whose Kind
// is "user" is a valid user-targeted delivery handle; ID is never an email
// address.
type AgentMember struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Role Access `json:"role"`
}

// ListMembers returns the principals currently granted access to this agent.
// The agent token is authorized only for its own membership roster; it cannot
// enumerate users or memberships belonging to another agent.
func (a *Agent) ListMembers(ctx context.Context) ([]AgentMember, error) {
	if !a.runtimeAvailable() {
		return nil, a.runtimeUnavailable("ListMembers")
	}
	var response struct {
		Members []AgentMember `json:"members"`
	}
	if err := a.client.doJSON(ctx, http.MethodGet, "/api/agent/members", nil, &response); err != nil {
		return nil, err
	}
	if response.Members == nil {
		return []AgentMember{}, nil
	}
	return response.Members, nil
}
