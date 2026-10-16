package agentsdk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/airlockrun/agentsdk/wire"
	"github.com/airlockrun/goai/schema"
)

// AgentReview declares durable application evaluation of typed submissions.
// Delivery is at least once; deduplicate external effects using submission ID.
type AgentReview[In, Out any] struct {
	Version        int
	Timeout        time.Duration
	MaxAttempts    int
	MaxConcurrency int
	MaxCorrections int
	Handler        func(context.Context, AgentReviewContext, AgentSubmission[In, Out]) (AgentReviewDecision, error)
}

// AgentArtifact identifies the immutable content pinned by the host at submission.
type AgentArtifact = wire.AgentArtifact

// AgentReviewDecision selects acceptance, terminal rejection, or correction.
type AgentReviewDecision = wire.AgentReviewDecision

// Accept publishes this submission as the subject run's completed output.
func Accept() AgentReviewDecision { return AgentReviewDecision{Kind: "accept"} }

// Fail terminates the subject run with a nonblank reason of at most 4096 bytes.
func Fail(reason string) AgentReviewDecision {
	return AgentReviewDecision{Kind: "fail", Feedback: reason}
}

// Continue requests correction on the same run with bounded nonblank feedback.
func Continue(feedback string) AgentReviewDecision {
	return AgentReviewDecision{Kind: "continue", Feedback: feedback}
}

// AgentSubmission contains fresh typed copies of the original admitted input
// and this immutable output revision.
type AgentSubmission[In, Out any] struct {
	ID           string
	RunID        string
	SessionID    string
	ToolCallID   string
	ContractHash string
	Revision     int64
	Input        In
	Output       Out
	Artifacts    []AgentArtifact
	Decision     *AgentReviewDecision
}

// AgentReviewContext binds exact artifact reads to one authenticated delivery.
type AgentReviewContext struct {
	ID             string // submission ID, stable across redelivery
	RunID          string // subject agent run, distinct from the callback execution run
	SessionID      string
	Revision       int64
	Job            JobContext
	agent          *Agent
	definition     string
	executionRunID string
	leaseToken     string
	artifacts      []AgentArtifact
}

// Open reads the pinned bytes for a manifest entry, even when its path changes.
// The supplied context must retain this exact job attempt and execution fence.
func (review AgentReviewContext) Open(ctx context.Context, artifact AgentArtifact) (io.ReadCloser, error) {
	active := jobRunFromContext(ctx)
	r := runFromContext(ctx)
	if active == nil || r == nil || review.agent == nil || active.agent != review.agent || r.agent != review.agent || r.id != review.executionRunID || active.id != review.Job.ID || active.attempt != review.Job.Attempt || active.leaseToken == "" || active.leaseToken != review.leaseToken {
		return nil, errors.New("agentsdk: artifact read requires the matching review delivery context")
	}
	index := -1
	for i, candidate := range review.artifacts {
		if candidate == artifact {
			index = i
			break
		}
	}
	if index < 0 {
		return nil, errors.New("agentsdk: artifact is not in this submission manifest")
	}
	path := "/api/agent/agents/" + url.PathEscape(review.definition) + "/submissions/" + url.PathEscape(review.ID) + "/artifacts/" + strconv.Itoa(index)
	resp, err := review.agent.client.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("agentsdk: open submission artifact: status %d: %s", resp.StatusCode, body)
	}
	return resp.Body, nil
}

func agentReviewPolicy[In, Out any](slug string, review *AgentReview[In, Out]) *wire.AgentReviewPolicy {
	if review.Handler == nil || review.Version <= 0 || review.Version > math.MaxInt32 || review.Timeout <= 0 || review.Timeout > maxJobTimeout || review.Timeout%time.Millisecond != 0 || review.MaxAttempts <= 0 || review.MaxAttempts > maxJobAttempts || review.MaxConcurrency <= 0 || review.MaxConcurrency > maxJobConcurrency || review.MaxCorrections < 0 || review.MaxCorrections > math.MaxInt32 {
		panic("agentsdk: RegisterAgent: invalid review policy or missing Handler")
	}
	return &wire.AgentReviewPolicy{Version: int32(review.Version), TimeoutMS: review.Timeout.Milliseconds(), MaxAttempts: int32(review.MaxAttempts), MaxConcurrency: int32(review.MaxConcurrency), MaxCorrections: int32(review.MaxCorrections), JobName: wire.AgentReviewJobName(slug)}
}

func registerAgentReview[In, Out any](a *Agent, d wire.AgentDefinition, review AgentReview[In, Out]) {
	// RawMessage fields encode JSON values, not byte arrays. Reflect the fixed
	// envelope and explicitly expose these host-supplied contract values.
	var envelope map[string]any
	if err := json.Unmarshal(schema.MustFromType(wire.AgentReviewRequest{}).MustJSON(), &envelope); err != nil {
		panic(err)
	}
	properties := envelope["properties"].(map[string]any)["submission"].(map[string]any)["properties"].(map[string]any)
	properties["input"], properties["output"] = true, true
	inputSchema, err := json.Marshal(envelope)
	if err != nil {
		panic(err)
	}
	registerJob(a, &Job[wire.AgentReviewRequest, wire.AgentReviewDecision]{
		Name: d.Review.JobName, Version: review.Version, Description: "Review submissions for " + d.Slug,
		Timeout: review.Timeout, MaxAttempts: review.MaxAttempts, MaxConcurrency: review.MaxConcurrency,
		Handler: func(ctx context.Context, job JobContext, request wire.AgentReviewRequest) (wire.AgentReviewDecision, error) {
			r := runFromContext(ctx)
			active := jobRunFromContext(ctx)
			s := request.Submission
			if r == nil || active == nil || r.agent != a || active.agent != a || active.id != job.ID || active.attempt != job.Attempt || active.leaseToken == "" || request.Definition != d.Slug || request.ContractHash != d.ContractHash || s.ContractHash != d.ContractHash || s.Revision < 1 || s.ToolCallID == "" || s.Decision != nil {
				return wire.AgentReviewDecision{}, errors.New("agentsdk: invalid review delivery binding")
			}
			for _, id := range []string{s.ID, s.RunID, s.SessionID, r.id} {
				if err := validateJobID(id); err != nil {
					return wire.AgentReviewDecision{}, err
				}
			}
			if s.RunID == r.id {
				return wire.AgentReviewDecision{}, errors.New("agentsdk: review execution must be distinct from the subject run")
			}
			input, err := decodeReviewValue[In](s.Input)
			if err != nil {
				return wire.AgentReviewDecision{}, fmt.Errorf("agentsdk: review input: %w", err)
			}
			output, err := decodeReviewValue[Out](s.Output)
			if err != nil {
				return wire.AgentReviewDecision{}, fmt.Errorf("agentsdk: review output: %w", err)
			}
			// Bind to the callback execution run without weakening Input's equality
			// fence. The authenticated host supplied the original subject input.
			ctx = context.WithValue(ctx, taskInputCtxKey{}, taskInputSnapshot{agent: a, runID: r.id, definition: wire.RuntimeAgentDefinition{Slug: d.Slug, ContractHash: d.ContractHash}, input: append(json.RawMessage(nil), s.Input...)})
			rc := AgentReviewContext{ID: s.ID, RunID: s.RunID, SessionID: s.SessionID, Revision: s.Revision, Job: job, agent: a, definition: d.Slug, executionRunID: r.id, leaseToken: active.leaseToken, artifacts: append([]AgentArtifact(nil), s.Artifacts...)}
			decision, err := review.Handler(ctx, rc, AgentSubmission[In, Out]{ID: s.ID, RunID: s.RunID, SessionID: s.SessionID, ToolCallID: s.ToolCallID, ContractHash: s.ContractHash, Revision: s.Revision, Input: input, Output: output, Artifacts: append([]AgentArtifact(nil), s.Artifacts...)})
			if err != nil {
				return wire.AgentReviewDecision{}, err
			}
			if err := validateReviewDecision(decision); err != nil {
				return wire.AgentReviewDecision{}, err
			}
			return decision, nil
		},
	}, inputSchema)
}

func decodeReviewValue[T any](raw json.RawMessage) (T, error) {
	var value T
	if err := validateAgentTypeValue(reflect.TypeFor[T](), raw); err != nil {
		return value, err
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	err := d.Decode(&value)
	return value, err
}

func validateReviewDecision(decision AgentReviewDecision) error {
	switch decision.Kind {
	case "accept":
		if decision.Feedback == "" {
			return nil
		}
	case "fail", "continue":
		if strings.TrimSpace(decision.Feedback) != "" && len(decision.Feedback) <= 4096 {
			return nil
		}
	}
	return errors.New("agentsdk: review decision requires accept without feedback or fail/continue with nonempty feedback of at most 4096 bytes")
}
