package agentsdk

import (
	"context"

	"github.com/airlockrun/agentsdk/wire"
	"github.com/airlockrun/sol/session"
)

// httpSessionStore implements session.SessionStore by calling Airlock's API.
// It is pre-scoped to a single conversation at construction time.
type httpSessionStore struct {
	client *airlockClient
	convID string
	runID  string
	source string // "user", "system" — passed as query param to Airlock
}

func newHTTPSessionStore(client *airlockClient, convID, runID, source string) *httpSessionStore {
	return &httpSessionStore{client: client, convID: convID, runID: runID, source: source}
}

func (s *httpSessionStore) Load(ctx context.Context) ([]session.Message, error) {
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

func (s *httpSessionStore) Append(ctx context.Context, msgs []session.Message) error {
	path := "/api/agent/session/" + s.convID + "/messages?runId=" + s.runID
	if s.source != "" {
		path += "&source=" + s.source
	}
	return s.client.doJSON(ctx, "POST", path, msgs, nil)
}

func (s *httpSessionStore) Compact(ctx context.Context, summary []session.Message, tokensFreed int) error {
	body := wire.SessionCompactRequest{Summary: summary, TokensFreed: tokensFreed}
	return s.client.doJSON(ctx, "POST", "/api/agent/session/"+s.convID+"/compact", body, nil)
}
