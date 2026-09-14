package agentsdk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/airlockrun/agentsdk/wire"
)

type AgentRunStatus string

const (
	AgentRunQueued         AgentRunStatus = "queued"
	AgentRunRunning        AgentRunStatus = "running"
	AgentRunWaiting        AgentRunStatus = "waiting"
	AgentRunCompleted      AgentRunStatus = "completed"
	AgentRunFailed         AgentRunStatus = "failed"
	AgentRunCancelled      AgentRunStatus = "cancelled"
	AgentRunBudgetExceeded AgentRunStatus = "budget_exceeded"
)

func (s AgentRunStatus) Terminal() bool {
	switch s {
	case AgentRunCompleted, AgentRunFailed, AgentRunCancelled, AgentRunBudgetExceeded:
		return true
	}
	return false
}

type AgentReplyKind string

const (
	AgentReplyOutput     AgentReplyKind = "output"
	AgentReplyNeedsInput AgentReplyKind = "needs_input"
)

// AgentReply is a completed task's typed output or question. Both kinds finish
// the run; either completed session can receive a Continue prompt.
type AgentReply[Out any] struct {
	Kind     AgentReplyKind
	Output   *Out
	Question string
}

type AgentRunInfo[Out any] struct {
	ID           string
	SessionID    string
	Definition   string
	ContractHash string
	Status       AgentRunStatus
	Error        string
	Reply        *AgentReply[Out]
	Steps        int64
	Tokens       int64
	CreatedAt    time.Time
	UpdatedAt    time.Time
	StartedAt    *time.Time
	CompletedAt  *time.Time
	Created      bool
}

type ListAgentRunsOptions struct {
	Limit     int
	Cursor    string
	Status    AgentRunStatus
	SessionID string
}

type AgentRunPage[Out any] struct {
	Runs       []AgentRunInfo[Out]
	NextCursor string
}

// Start creates a fresh application-owned session. Persist requestID (a
// canonical non-nil UUID) before calling and reuse it with identical input when
// retrying an uncertain response. No human or rolling background run is minted.
func (h *AgentHandle[In, Out]) Start(ctx context.Context, requestID string, input In) (AgentRunInfo[Out], error) {
	d := h.registeredAgent().definition
	if err := validateJobID(requestID); err != nil {
		return AgentRunInfo[Out]{}, err
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return AgentRunInfo[Out]{}, fmt.Errorf("agentsdk: encode agent input: %w", err)
	}
	return h.request(ctx, "POST", "/runs", wire.StartAgentRequest{RequestID: requestID, ContractHash: d.ContractHash, Input: raw}, "", "")
}

// Get observes a persisted run by ID without creating an execution context.
func (h *AgentHandle[In, Out]) Get(ctx context.Context, id string) (AgentRunInfo[Out], error) {
	if err := validateJobID(id); err != nil {
		return AgentRunInfo[Out]{}, err
	}
	return h.request(ctx, "GET", "/runs/"+id, nil, id, "")
}

// Cancel requests cancellation and returns the durable state reported by the host.
func (h *AgentHandle[In, Out]) Cancel(ctx context.Context, id string) (AgentRunInfo[Out], error) {
	if err := validateJobID(id); err != nil {
		return AgentRunInfo[Out]{}, err
	}
	return h.request(ctx, "DELETE", "/runs/"+id, nil, id, "")
}

// Continue starts a new run in a completed session, for either reply kind.
// requestID supplies the same retry guarantee as Start. The host checks that
// the session is completed and bound to this exact contract.
func (h *AgentHandle[In, Out]) Continue(ctx context.Context, sessionID, requestID, prompt string) (AgentRunInfo[Out], error) {
	d := h.registeredAgent().definition
	for _, id := range []string{sessionID, requestID} {
		if err := validateJobID(id); err != nil {
			return AgentRunInfo[Out]{}, err
		}
	}
	if strings.TrimSpace(prompt) == "" {
		return AgentRunInfo[Out]{}, errors.New("agentsdk: continuation prompt is required")
	}
	return h.request(ctx, "POST", "/sessions/"+sessionID+"/continue", wire.ContinueAgentRequest{RequestID: requestID, ContractHash: d.ContractHash, Prompt: prompt}, "", sessionID)
}

// List returns a cursor page of runs matching this handle's current contract.
// The host filters by contract hash before applying the page limit.
func (h *AgentHandle[In, Out]) List(ctx context.Context, opts ListAgentRunsOptions) (AgentRunPage[Out], error) {
	d := h.registeredAgent().definition
	h.agent.requireRuntime("AgentHandle.List")
	if opts.Limit < 0 {
		return AgentRunPage[Out]{}, errors.New("agentsdk: list limit must be nonnegative")
	}
	q := url.Values{"contractHash": {d.ContractHash}}
	if opts.Limit > 0 {
		q.Set("limit", strconv.Itoa(opts.Limit))
	}
	if opts.Cursor != "" {
		q.Set("cursor", opts.Cursor)
	}
	if opts.Status != "" {
		if !validAgentRunStatus(opts.Status) {
			return AgentRunPage[Out]{}, errors.New("agentsdk: invalid agent run status")
		}
		q.Set("status", string(opts.Status))
	}
	if opts.SessionID != "" {
		if err := validateJobID(opts.SessionID); err != nil {
			return AgentRunPage[Out]{}, err
		}
		q.Set("sessionId", opts.SessionID)
	}
	path := "/api/agent/agents/" + d.Slug + "/runs?" + q.Encode()
	var response wire.ListAgentRunsResponse
	if err := h.agent.client.doJSON(ctx, "GET", path, nil, &response); err != nil {
		return AgentRunPage[Out]{}, err
	}
	if response.Runs == nil {
		return AgentRunPage[Out]{}, errors.New("agentsdk: agent list response requires a runs array")
	}
	page := AgentRunPage[Out]{Runs: make([]AgentRunInfo[Out], 0, len(response.Runs)), NextCursor: response.NextCursor}
	for _, run := range response.Runs {
		info, err := agentRunInfo[Out](d, run, false)
		if err != nil {
			return AgentRunPage[Out]{}, err
		}
		if (opts.SessionID != "" && info.SessionID != opts.SessionID) || (opts.Status != "" && info.Status != opts.Status) {
			return AgentRunPage[Out]{}, errors.New("agentsdk: agent list response filter mismatch")
		}
		page.Runs = append(page.Runs, info)
	}
	return page, nil
}

// Wait polls a persisted run until terminal. Cancelling ctx stops observation,
// never the run. Terminal failures are returned as run state, not transport errors.
func (h *AgentHandle[In, Out]) Wait(ctx context.Context, id string) (AgentRunInfo[Out], error) {
	for {
		info, err := h.Get(ctx, id)
		if err != nil {
			return AgentRunInfo[Out]{}, err
		}
		if info.Status.Terminal() {
			return info, nil
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return AgentRunInfo[Out]{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func (h *AgentHandle[In, Out]) request(ctx context.Context, method, suffix string, body any, id, sessionID string) (AgentRunInfo[Out], error) {
	d := h.registeredAgent().definition
	h.agent.requireRuntime("AgentHandle." + method)
	var response wire.AgentRunResponse
	if err := h.agent.client.doJSON(ctx, method, "/api/agent/agents/"+d.Slug+suffix, body, &response); err != nil {
		return AgentRunInfo[Out]{}, err
	}
	info, err := agentRunInfo[Out](d, response.Run, response.Created)
	if err != nil {
		return AgentRunInfo[Out]{}, err
	}
	if (id != "" && id != info.ID) || (sessionID != "" && sessionID != info.SessionID) {
		return AgentRunInfo[Out]{}, errors.New("agentsdk: agent response ID mismatch")
	}
	return info, nil
}

func validAgentRunStatus(status AgentRunStatus) bool {
	return status == AgentRunQueued || status == AgentRunRunning || status == AgentRunWaiting || status.Terminal()
}

func agentRunInfo[Out any](d wire.AgentDefinition, r wire.AgentRunInfo, created bool) (AgentRunInfo[Out], error) {
	if r.Definition != d.Slug || r.ContractHash != d.ContractHash {
		return AgentRunInfo[Out]{}, errors.New("agentsdk: agent response contract mismatch")
	}
	for _, id := range []string{r.ID, r.SessionID} {
		if err := validateJobID(id); err != nil {
			return AgentRunInfo[Out]{}, fmt.Errorf("agentsdk: invalid agent response ID: %w", err)
		}
	}
	status := AgentRunStatus(r.Status)
	if !validAgentRunStatus(status) || r.Steps < 0 || r.Tokens < 0 {
		return AgentRunInfo[Out]{}, errors.New("agentsdk: invalid agent response state")
	}
	info := AgentRunInfo[Out]{ID: r.ID, SessionID: r.SessionID, Definition: r.Definition, ContractHash: r.ContractHash,
		Status: status, Error: r.Error, Steps: r.Steps, Tokens: r.Tokens, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
		StartedAt: r.StartedAt, CompletedAt: r.CompletedAt, Created: created}
	if status != AgentRunCompleted {
		if r.Reply != nil {
			return AgentRunInfo[Out]{}, errors.New("agentsdk: non-completed agent run has a reply")
		}
		return info, nil
	}
	if r.Reply == nil {
		return AgentRunInfo[Out]{}, errors.New("agentsdk: completed agent run has no reply")
	}
	reply := &AgentReply[Out]{Kind: AgentReplyKind(r.Reply.Kind), Question: r.Reply.Question}
	switch reply.Kind {
	case AgentReplyOutput:
		if len(r.Reply.Output) == 0 || reply.Question != "" {
			return AgentRunInfo[Out]{}, errors.New("agentsdk: invalid output reply")
		}
		if err := validateAgentTypeValue(reflect.TypeFor[Out](), r.Reply.Output); err != nil {
			return AgentRunInfo[Out]{}, fmt.Errorf("agentsdk: validate agent output: %w", err)
		}
		var output Out
		decoder := json.NewDecoder(bytes.NewReader(r.Reply.Output))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&output); err != nil {
			return AgentRunInfo[Out]{}, fmt.Errorf("agentsdk: decode agent output: %w", err)
		}
		reply.Output = &output
	case AgentReplyNeedsInput:
		if len(r.Reply.Output) != 0 || strings.TrimSpace(reply.Question) == "" {
			return AgentRunInfo[Out]{}, errors.New("agentsdk: invalid needs_input reply")
		}
	default:
		return AgentRunInfo[Out]{}, errors.New("agentsdk: invalid agent reply kind")
	}
	info.Reply = reply
	return info, nil
}
