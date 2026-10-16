package agentsdk

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/airlockrun/agentsdk/agentruntime"
	"github.com/airlockrun/agentsdk/wire"
	"github.com/google/uuid"
)

func reviewDefinition() *AgentDefinition[taskInput, taskOutput] {
	d := taskDefinition()
	d.Review = &AgentReview[taskInput, taskOutput]{Version: 1, Timeout: time.Second, MaxAttempts: 2, MaxConcurrency: 1, MaxCorrections: 2, Handler: func(context.Context, AgentReviewContext, AgentSubmission[taskInput, taskOutput]) (AgentReviewDecision, error) {
		return Accept(), nil
	}}
	return d
}

func TestAgentReviewRegistrationSnapshot(t *testing.T) {
	a := New(Config{Description: "Review test"})
	taskModel(a)
	d := reviewDefinition()
	h := RegisterAgent(a, d)
	before := a.buildManifest()
	d.Review.Version = 2
	d.Review.MaxCorrections = 9
	d.Review.Handler = nil
	manifest := a.Manifest()
	if !reflect.DeepEqual(before, manifest) {
		t.Fatal("mutable reviewer registration")
	}
	r := manifest.AgentDefinitions[0].Review
	if r.JobName != wire.AgentReviewJobName(h.Slug()) || len(manifest.JobHandlers) != 1 {
		t.Fatalf("review=%+v jobs=%+v", r, manifest.JobHandlers)
	}
	def := manifest.AgentDefinitions[0]
	def.Review.MaxCorrections++
	hash, err := wire.AgentDefinitionHash(def)
	if err != nil || hash == h.ContractHash() {
		t.Fatalf("review missing from hash: %v", err)
	}
}

func TestAgentReviewValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*AgentReview[taskInput, taskOutput])
	}{
		{"version", func(r *AgentReview[taskInput, taskOutput]) { r.Version = 0 }},
		{"timeout", func(r *AgentReview[taskInput, taskOutput]) { r.Timeout = 0 }},
		{"fractional timeout", func(r *AgentReview[taskInput, taskOutput]) { r.Timeout = time.Millisecond + time.Nanosecond }},
		{"attempts", func(r *AgentReview[taskInput, taskOutput]) { r.MaxAttempts = 0 }},
		{"concurrency", func(r *AgentReview[taskInput, taskOutput]) { r.MaxConcurrency = 0 }},
		{"corrections", func(r *AgentReview[taskInput, taskOutput]) { r.MaxCorrections = -1 }},
		{"handler", func(r *AgentReview[taskInput, taskOutput]) { r.Handler = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := New(Config{Description: "Test"})
			taskModel(a)
			d := reviewDefinition()
			tc.edit(d.Review)
			defer func() {
				if recover() == nil {
					t.Fatal("invalid review accepted")
				}
			}()
			RegisterAgent(a, d)
		})
	}
}

func TestAgentReviewBoundInputAndExactArtifactRead(t *testing.T) {
	a := New(Config{Description: "Test"})
	taskModel(a)
	var h *AgentHandle[taskInput, taskOutput]
	d := reviewDefinition()
	executionID, jobID, lease := uuid.NewString(), uuid.NewString(), uuid.NewString()
	artifact := AgentArtifact{Path: "reports/result.json", ID: uuid.NewString(), ContentID: "pinned", SHA256: "digest", Size: 6, ContentType: "application/json"}
	submissionID := uuid.NewString()
	reads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		reads++
		if req.URL.Path != "/api/agent/agents/worker/submissions/"+submissionID+"/artifacts/0" || req.Header.Get("X-Airlock-Run-ID") != executionID || req.Header.Get("X-Airlock-Job-ID") != jobID || req.Header.Get("X-Airlock-Job-Attempt") != "2" || req.Header.Get(jobLeaseTokenHeader) != lease || req.Header.Get("Authorization") != "Bearer token" {
			t.Errorf("unfenced request: %s %+v", req.URL, req.Header)
		}
		_, _ = io.WriteString(w, "pinned")
	}))
	defer server.Close()
	a.client = newAirlockClient(server.URL, "token", server.Client())
	d.Review.Handler = func(ctx context.Context, review AgentReviewContext, s AgentSubmission[taskInput, taskOutput]) (AgentReviewDecision, error) {
		input, err := h.Input(ctx)
		if err != nil {
			return AgentReviewDecision{}, err
		}
		if input.Task != "original" || s.Input.Task != input.Task || s.Output.Answer != "draft" || review.RunID != s.RunID {
			t.Fatal("typed subject binding lost")
		}
		s.Artifacts[0].Path = "mutated"
		if _, err := review.Open(ctx, s.Artifacts[0]); err == nil {
			t.Fatal("changed manifest accepted")
		}
		if _, err := review.Open(context.Background(), artifact); err == nil {
			t.Fatal("unbound read accepted")
		}
		stale := contextWithJobRun(ctx, &jobRunContext{agent: a, id: jobID, attempt: 1, leaseToken: lease})
		if _, err := review.Open(stale, artifact); err == nil {
			t.Fatal("stale attempt accepted")
		}
		reader, err := review.Open(ctx, artifact)
		if err != nil {
			return AgentReviewDecision{}, err
		}
		defer reader.Close()
		body, err := io.ReadAll(reader)
		if err != nil {
			return AgentReviewDecision{}, err
		}
		if string(body) != "pinned" {
			t.Fatal(string(body))
		}
		return Continue("Correct the result"), nil
	}
	h = RegisterAgent(a, d)
	job := a.jobs[jobKey{name: wire.AgentReviewJobName(h.Slug()), version: 1}]
	r := newRun(a, executionID, "", "", t.Context())
	ctx := contextWithRun(t.Context(), r)
	ctx = contextWithJobRun(ctx, &jobRunContext{agent: a, id: jobID, attempt: 2, leaseToken: lease})
	r.ctx = ctx
	req := wire.AgentReviewRequest{Definition: h.Slug(), ContractHash: h.ContractHash(), Submission: wire.AgentSubmission{ID: submissionID, RunID: uuid.NewString(), SessionID: uuid.NewString(), ToolCallID: "submit-call", ContractHash: h.ContractHash(), Revision: 1, Input: json.RawMessage(`{"task":"original"}`), Output: json.RawMessage(`{"answer":"draft"}`), Artifacts: []wire.AgentArtifact{artifact}}}
	raw, _ := json.Marshal(req)
	if err := agentruntime.ValidateJSON(job.inputSchema, raw); err != nil {
		t.Fatalf("derived review schema: %s: %v", job.inputSchema, err)
	}
	for i := 0; i < 2; i++ {
		out, err := job.handler(ctx, JobContext{ID: jobID, Attempt: 2}, raw)
		if err != nil || string(out) != `{"kind":"continue","feedback":"Correct the result"}` {
			t.Fatalf("delivery %d: %s %v", i, out, err)
		}
	}
	if reads != 2 {
		t.Fatalf("reads=%d", reads)
	}
	req.ContractHash = "foreign"
	raw, _ = json.Marshal(req)
	if _, err := job.handler(ctx, JobContext{ID: jobID, Attempt: 2}, raw); err == nil {
		t.Fatal("foreign contract accepted")
	}
}

func TestAgentReviewDecisions(t *testing.T) {
	for _, tc := range []struct {
		name     string
		decision AgentReviewDecision
		valid    bool
	}{
		{"accept", Accept(), true}, {"fail", Fail("Invalid"), true}, {"continue", Continue("Correct"), true},
		{"empty fail", Fail(""), false}, {"blank continue", Continue("  "), false}, {"accept feedback", AgentReviewDecision{Kind: "accept", Feedback: "why"}, false}, {"unknown", AgentReviewDecision{Kind: "output"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if (validateReviewDecision(tc.decision) == nil) != tc.valid {
				t.Fatal(tc.decision)
			}
		})
	}
}
