package agentsdk

import (
	"context"
	"errors"
	"sync"

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

	mu       sync.Mutex
	revision string
}

func newHTTPSessionStore(client *airlockClient, convID, runID, source string) *httpSessionStore {
	return &httpSessionStore{client: client, convID: convID, runID: runID, source: source}
}

func (s *httpSessionStore) Load(ctx context.Context) ([]session.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var response wire.SessionLoadResponse
	err := s.client.doJSON(ctx, "GET", "/api/agent/session/"+s.convID+"/messages", nil, &response)
	if err != nil {
		return nil, err
	}
	if response.Revision == "" {
		return nil, errors.New("agentsdk: session load response is missing revision")
	}
	s.revision = response.Revision
	if response.Messages == nil {
		response.Messages = []session.Message{}
	}
	return response.Messages, nil
}

func (s *httpSessionStore) Append(ctx context.Context, msgs []session.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.revision == "" {
		return errors.New("agentsdk: session append requires a prior load")
	}

	path := "/api/agent/session/" + s.convID + "/messages?runId=" + s.runID
	if s.source != "" {
		path += "&source=" + s.source
	}
	var response wire.SessionAppendResponse
	if err := s.client.doJSON(ctx, "POST", path, wire.SessionAppendRequest{
		Messages: msgs,
		Revision: s.revision,
	}, &response); err != nil {
		return err
	}
	if response.Revision == "" {
		return errors.New("agentsdk: session append response is missing revision")
	}
	s.revision = response.Revision
	return nil
}

func (s *httpSessionStore) Compact(ctx context.Context, summary []session.Message, tokensFreed int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.revision == "" {
		return errors.New("agentsdk: session compact requires a prior load")
	}

	body := wire.SessionCompactRequest{Summary: summary, TokensFreed: tokensFreed, Revision: s.revision}
	var response wire.SessionCompactResponse
	if err := s.client.doJSON(ctx, "POST", "/api/agent/session/"+s.convID+"/compact", body, &response); err != nil {
		return err
	}
	if response.Revision == "" {
		return errors.New("agentsdk: session compact response is missing revision")
	}
	s.revision = response.Revision
	return nil
}
