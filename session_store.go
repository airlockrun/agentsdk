package agentsdk

import (
	"context"

	"github.com/airlockrun/sol/session"
)

// HTTPSessionStore implements session.SessionStore by calling Airlock's API.
// It is pre-scoped to a single conversation at construction time.
type HTTPSessionStore struct {
	client *airlockClient
	convID string
	runID  string
	source string // "user", "system" — passed as query param to Airlock
}

// NewHTTPSessionStore creates a store scoped to the given conversation.
func NewHTTPSessionStore(client *airlockClient, convID, runID, source string) *HTTPSessionStore {
	return &HTTPSessionStore{client: client, convID: convID, runID: runID, source: source}
}

func (s *HTTPSessionStore) Load(ctx context.Context) ([]session.Message, error) {
	var msgs []session.Message
	err := s.client.doJSON(ctx, "GET", "/api/agent/session/"+s.convID+"/messages", nil, &msgs)
	if err != nil {
		return nil, err
	}
	if msgs == nil {
		msgs = []session.Message{}
	}
	return msgs, nil
}

func (s *HTTPSessionStore) Append(ctx context.Context, msgs []session.Message) error {
	path := "/api/agent/session/" + s.convID + "/messages?runId=" + s.runID
	if s.source != "" {
		path += "&source=" + s.source
	}
	return s.client.doJSON(ctx, "POST", path, msgs, nil)
}

// compactRequest is the wire format for POST /api/agent/session/{convID}/compact.
type compactRequest struct {
	Summary     []session.Message `json:"summary"`
	TokensFreed int               `json:"tokensFreed"`
}

func (s *HTTPSessionStore) Compact(ctx context.Context, summary []session.Message, tokensFreed int) error {
	body := compactRequest{Summary: summary, TokensFreed: tokensFreed}
	return s.client.doJSON(ctx, "POST", "/api/agent/session/"+s.convID+"/compact", body, nil)
}
