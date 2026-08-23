package agentsdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/airlockrun/agentsdk/wire"
)

const testJobID = "11111111-1111-1111-1111-111111111111"

const (
	testJobRunID        = "22222222-2222-2222-2222-222222222222"
	testJobUserID       = "33333333-3333-3333-3333-333333333333"
	testJobConversation = "44444444-4444-4444-4444-444444444444"
	testJobLeaseToken   = "55555555-5555-5555-5555-555555555555"
)

type testJobInput struct {
	Source string `json:"source" description:"Object-storage source path"`
}

type testJobOutput struct {
	Result string `json:"result"`
}

func testJobDefinition(version int) *Job[testJobInput, testJobOutput] {
	return &Job[testJobInput, testJobOutput]{
		Name:           "convert_video",
		Version:        version,
		Description:    "Convert a stored video.",
		Timeout:        time.Minute,
		MaxAttempts:    3,
		MaxConcurrency: 2,
		Handler: func(context.Context, JobContext, testJobInput) (testJobOutput, error) {
			return testJobOutput{}, nil
		},
	}
}

func TestRegisterJob(t *testing.T) {
	a, _ := testAgent(t)
	handle := RegisterJob(a, testJobDefinition(1))
	RegisterJob(a, testJobDefinition(2))

	if handle.Name() != "convert_video" || handle.Version() != 1 {
		t.Fatalf("handle = %s@v%d", handle.Name(), handle.Version())
	}
	if len(a.jobs) != 2 {
		t.Fatalf("registered jobs = %d, want 2", len(a.jobs))
	}
	job := a.jobs[jobKey{name: "convert_video", version: 1}]
	if job == nil {
		t.Fatal("convert_video@v1 was not registered")
	}
	if len(job.inputSchemaHash) != 64 || len(job.outputSchemaHash) != 64 {
		t.Fatalf("schema hashes = %q, %q", job.inputSchemaHash, job.outputSchemaHash)
	}
	if job.inputSchemaHash != "62c1e55d75d60a515078c3b7838bae379363ab2f037a63495abdd1245317b4e6" {
		t.Fatalf("input schema hash = %s", job.inputSchemaHash)
	}
	if job.outputSchemaHash != "d1c0152234e3725b6ed42c9de8fb5f60feeb3fd3dd8a5494df33021905baa990" {
		t.Fatalf("output schema hash = %s", job.outputSchemaHash)
	}
	if !strings.Contains(string(job.inputSchema), `"source"`) {
		t.Fatalf("input schema = %s", job.inputSchema)
	}
}

func TestRegisterJobRejectsDuplicateContract(t *testing.T) {
	a, _ := testAgent(t)
	RegisterJob(a, testJobDefinition(1))
	defer func() {
		if got := recover(); got == nil || !strings.Contains(got.(string), "duplicate RegisterJob") {
			t.Fatalf("panic = %v, want duplicate RegisterJob", got)
		}
	}()
	RegisterJob(a, testJobDefinition(1))
}

func TestJobHandleCronRegistersExactContractAndInput(t *testing.T) {
	a, _ := testAgent(t)
	RegisterJob(a, testJobDefinition(1))
	handle := RegisterJob(a, testJobDefinition(2))
	input := testJobInput{Source: "uploads/daily.mov"}
	handle.Cron(&JobCron[testJobInput]{
		Slug:        "daily_conversion",
		Schedule:    "0 9 * * *",
		Input:       input,
		Description: "Convert the daily upload.",
	})
	input.Source = "mutated"

	cron := a.jobCrons["daily_conversion"]
	job := a.jobs[jobKey{name: "convert_video", version: 2}]
	if cron == nil {
		t.Fatal("cron was not registered")
	}
	if cron.handlerName != job.name || cron.handlerVersion != job.version ||
		cron.inputSchemaHash != job.inputSchemaHash || cron.outputSchemaHash != job.outputSchemaHash {
		t.Fatalf("cron contract = %+v", cron)
	}
	if string(cron.input) != `{"source":"uploads/daily.mov"}` {
		t.Fatalf("cron input = %s", cron.input)
	}
}

func TestJobHandleCronRejectsDuplicateSlugAgentWide(t *testing.T) {
	a, _ := testAgent(t)
	first := RegisterJob(a, testJobDefinition(1))
	second := RegisterJob(a, testJobDefinition(2))
	first.Cron(&JobCron[testJobInput]{Slug: "daily", Schedule: "@daily", Description: "Daily v1."})

	defer func() {
		got := recover()
		if got == nil || !strings.Contains(got.(string), "duplicate job cron slug") {
			t.Fatalf("panic = %v, want duplicate job cron slug", got)
		}
	}()
	second.Cron(&JobCron[testJobInput]{Slug: "daily", Schedule: "@daily", Description: "Daily v2."})
}

func TestJobHandleCronValidation(t *testing.T) {
	tests := []struct {
		name string
		cron *JobCron[testJobInput]
		want string
	}{
		{name: "nil", want: "nil *JobCron"},
		{name: "slug required", cron: &JobCron[testJobInput]{Schedule: "@daily", Description: "Daily."}, want: "lowercase snake_case"},
		{name: "slug format", cron: &JobCron[testJobInput]{Slug: "Daily-Job", Schedule: "@daily", Description: "Daily."}, want: "lowercase snake_case"},
		{name: "schedule", cron: &JobCron[testJobInput]{Slug: "daily", Schedule: "not a cron", Description: "Daily."}, want: "invalid Schedule"},
		{name: "description required", cron: &JobCron[testJobInput]{Slug: "daily", Schedule: "@daily"}, want: "Description is required"},
		{name: "description bounded", cron: &JobCron[testJobInput]{Slug: "daily", Schedule: "@daily", Description: strings.Repeat("x", maxJobDescriptionBytes+1)}, want: "Description exceeds 4096 bytes"},
		{name: "input bounded", cron: &JobCron[testJobInput]{Slug: "daily", Schedule: "@daily", Input: testJobInput{Source: strings.Repeat("x", maxJobPayloadBytes)}, Description: "Daily."}, want: "Input exceeds 65536 bytes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, _ := testAgent(t)
			handle := RegisterJob(a, testJobDefinition(1))
			defer func() {
				got := recover()
				if got == nil || !strings.Contains(got.(string), tt.want) {
					t.Fatalf("panic = %v, want %q", got, tt.want)
				}
			}()
			handle.Cron(tt.cron)
		})
	}
}

func TestJobHandleCronRejectsUnencodableInput(t *testing.T) {
	type input struct {
		Value float64 `json:"value"`
	}
	a, _ := testAgent(t)
	handle := RegisterJob(a, &Job[input, testJobOutput]{
		Name: "float_job", Version: 1, Description: "Process a float.", Timeout: time.Minute,
		MaxAttempts: 1, MaxConcurrency: 1,
		Handler: func(context.Context, JobContext, input) (testJobOutput, error) { return testJobOutput{}, nil },
	})
	defer func() {
		got := recover()
		if got == nil || !strings.Contains(got.(string), "encode Input") {
			t.Fatalf("panic = %v, want encode Input", got)
		}
	}()
	handle.Cron(&JobCron[input]{Slug: "invalid_float", Schedule: "@daily", Input: input{Value: math.NaN()}, Description: "Invalid float."})
}

func TestRegisterJobRequiresObjectInput(t *testing.T) {
	a, _ := testAgent(t)
	defer func() {
		if got := recover(); got == nil || !strings.Contains(got.(string), "input type must be a struct") {
			t.Fatalf("panic = %v, want struct input", got)
		}
	}()
	RegisterJob(a, &Job[string, testJobOutput]{
		Name: "invalid", Version: 1, Description: "Invalid input.", Timeout: time.Minute,
		MaxAttempts: 1, MaxConcurrency: 1,
		Handler: func(context.Context, JobContext, string) (testJobOutput, error) { return testJobOutput{}, nil },
	})
}

func TestRegisterJobRejectsNilHandler(t *testing.T) {
	a, _ := testAgent(t)
	job := testJobDefinition(1)
	job.Handler = nil
	defer func() {
		if got := recover(); got == nil || !strings.Contains(got.(string), "Handler is required") {
			t.Fatalf("panic = %v, want Handler is required", got)
		}
	}()
	RegisterJob(a, job)
}

func TestRegisterJobRejectsSubMillisecondTimeout(t *testing.T) {
	a, _ := testAgent(t)
	job := testJobDefinition(1)
	job.Timeout = time.Nanosecond
	defer func() {
		if got := recover(); got == nil || !strings.Contains(got.(string), "at least one millisecond") {
			t.Fatalf("panic = %v, want timeout wire limit", got)
		}
	}()
	RegisterJob(a, job)
}

func TestRegisterJobRejectsCustomJSONEncoding(t *testing.T) {
	type input struct {
		At time.Time `json:"at"`
	}
	a, _ := testAgent(t)
	defer func() {
		if got := recover(); got == nil || !strings.Contains(got.(string), "custom JSON encoding") {
			t.Fatalf("panic = %v, want custom JSON encoding", got)
		}
	}()
	RegisterJob(a, &Job[input, testJobOutput]{
		Name: "timestamped", Version: 1, Description: "Use a timestamp.", Timeout: time.Minute,
		MaxAttempts: 1, MaxConcurrency: 1,
		Handler: func(context.Context, JobContext, input) (testJobOutput, error) { return testJobOutput{}, nil },
	})
}

func TestCanonicalJobSchemaHash(t *testing.T) {
	left := canonicalJobSchema([]byte(`{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"integer"}},"required":["b","a"]}`))
	right := canonicalJobSchema([]byte(`{"required":["a","b"],"properties":{"b":{"type":"integer"},"a":{"type":"string"}},"type":"object"}`))
	if hashJobSchema(left) != hashJobSchema(right) {
		t.Fatalf("equivalent schemas hash differently: %s != %s", hashJobSchema(left), hashJobSchema(right))
	}
}

func TestRegisterJobRejectsUnsupportedJSONTag(t *testing.T) {
	type input struct {
		Count int `json:"count,string"`
	}
	a, _ := testAgent(t)
	defer func() {
		if got := recover(); got == nil || !strings.Contains(got.(string), "unsupported json option") {
			t.Fatalf("panic = %v, want unsupported json option", got)
		}
	}()
	RegisterJob(a, &Job[input, testJobOutput]{
		Name: "tagged", Version: 1, Description: "Use an encoded count.", Timeout: time.Minute,
		MaxAttempts: 1, MaxConcurrency: 1,
		Handler: func(context.Context, JobContext, input) (testJobOutput, error) { return testJobOutput{}, nil },
	})
}

func TestJobHandleEnqueue(t *testing.T) {
	a, mock := testAgent(t)
	handle := RegisterJob(a, testJobDefinition(1))

	result, err := handle.Enqueue(context.Background(), testJobID, testJobInput{Source: "uploads/video.mov"})
	if err != nil {
		t.Fatal(err)
	}
	if result.ID != testJobID || result.Status != JobStatusQueued || !result.Created || result.Output != nil {
		t.Fatalf("result = %+v", result)
	}
	createRequests := mock.RequestsByPath("/api/agent/run/create")
	if len(createRequests) != 1 {
		t.Fatalf("run/create requests = %d, want 1", len(createRequests))
	}
	var createRequest wire.CreateRunRequest
	if err := json.Unmarshal(createRequests[0].Body, &createRequest); err != nil {
		t.Fatal(err)
	}
	if createRequest.TriggerType != "background" {
		t.Fatalf("trigger type = %q, want background", createRequest.TriggerType)
	}

	requests := mock.RequestsByPath("/api/agent/jobs")
	if len(requests) != 1 {
		t.Fatalf("job requests = %d, want 1", len(requests))
	}
	if requests[0].Method != http.MethodPost {
		t.Fatalf("method = %s", requests[0].Method)
	}
	var request wire.EnqueueJobRequest
	if err := json.Unmarshal(requests[0].Body, &request); err != nil {
		t.Fatal(err)
	}
	job := a.jobs[jobKey{name: handle.name, version: handle.version}]
	if request.ID != testJobID || request.Name != job.name || request.Version != int32(job.version) ||
		request.InputSchemaHash != job.inputSchemaHash || request.OutputSchemaHash != job.outputSchemaHash || request.ScheduledAt != nil {
		t.Fatalf("request = %+v", request)
	}
	if string(request.Input) != `{"source":"uploads/video.mov"}` {
		t.Fatalf("input = %s", request.Input)
	}
	if got := requests[0].Header.Get("X-Airlock-Run-ID"); got != "run-mock-123" {
		t.Fatalf("X-Airlock-Run-ID = %q", got)
	}
	if got := requests[0].Header.Get("Authorization"); got != "Bearer test-token" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := requests[0].Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
}

func TestJobHandleEnqueueAt(t *testing.T) {
	a, mock := testAgent(t)
	handle := RegisterJob(a, testJobDefinition(1))
	fireAt := time.Date(2026, time.August, 22, 12, 30, 0, 123456789, time.FixedZone("offset", 3600))

	result, err := handle.EnqueueAt(context.Background(), testJobID, fireAt, testJobInput{Source: "uploads/video.mov"})
	if err != nil {
		t.Fatal(err)
	}
	if result.ID != testJobID || result.Status != JobStatusQueued || !result.Created {
		t.Fatalf("result = %+v", result)
	}
	requests := mock.RequestsByPath("/api/agent/jobs")
	if len(requests) != 1 {
		t.Fatalf("job requests = %d, want 1", len(requests))
	}
	var request wire.EnqueueJobRequest
	if err := json.Unmarshal(requests[0].Body, &request); err != nil {
		t.Fatal(err)
	}
	want := fireAt.UTC().Truncate(time.Microsecond)
	if request.ScheduledAt == nil || !request.ScheduledAt.Equal(want) || request.ScheduledAt.Location() != time.UTC {
		t.Fatalf("scheduledAt = %v, want %v", request.ScheduledAt, want)
	}
}

func TestJobHandleEnqueueAtRequiresFireAtBeforeCreatingRun(t *testing.T) {
	a, mock := testAgent(t)
	handle := RegisterJob(a, testJobDefinition(1))
	if _, err := handle.EnqueueAt(context.Background(), testJobID, time.Time{}, testJobInput{}); err == nil || !strings.Contains(err.Error(), "fireAt is required") {
		t.Fatalf("error = %v, want required fireAt", err)
	}
	if requests := mock.Requests(); len(requests) != 0 {
		t.Fatalf("requests = %+v, want none", requests)
	}
}

func TestJobHandleEnqueueMaterializesLazyRun(t *testing.T) {
	a, mock := testAgent(t)
	handle := RegisterJob(a, testJobDefinition(1))
	lazy := &lazyRun{agent: a, triggerRef: "POST /work", callerAccess: AccessUser}

	if _, err := handle.Enqueue(contextWithLazyRun(context.Background(), lazy), testJobID, testJobInput{}); err != nil {
		t.Fatal(err)
	}
	if lazy.materialized() == nil {
		t.Fatal("lazy run was not materialized")
	}
	requests := mock.RequestsByPath("/api/agent/run/create")
	if len(requests) != 1 {
		t.Fatalf("run/create requests = %d, want 1", len(requests))
	}
	var request wire.CreateRunRequest
	if err := json.Unmarshal(requests[0].Body, &request); err != nil {
		t.Fatal(err)
	}
	if request.TriggerType != "code" || request.TriggerRef != "POST /work" {
		t.Fatalf("request = %+v", request)
	}
}

func TestJobHandleEnqueueReturnsTypedUnavailableError(t *testing.T) {
	a, mock := testAgent(t)
	handle := RegisterJob(a, testJobDefinition(1))
	mock.EnqueueJobError = &wire.EnqueueJobErrorResponse{
		Code:           wire.EnqueueJobErrorCodeUnavailable,
		Error:          "candidate deployment does not provide this contract",
		HandlerName:    "convert_video",
		HandlerVersion: 1,
	}

	_, err := handle.Enqueue(context.Background(), testJobID, testJobInput{})
	if !errors.Is(err, ErrJobEnqueueUnavailable) {
		t.Fatalf("error = %v, want ErrJobEnqueueUnavailable", err)
	}
	var unavailable *JobEnqueueUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("error type = %T, want *JobEnqueueUnavailableError", err)
	}
	if unavailable.HandlerName != "convert_video" || unavailable.HandlerVersion != 1 || unavailable.Message != "candidate deployment does not provide this contract" {
		t.Fatalf("typed error = %+v", unavailable)
	}
}

func TestJobHandleRejectsInvalidIDAndPayloadBeforeCreatingRun(t *testing.T) {
	a, mock := testAgent(t)
	handle := RegisterJob(a, testJobDefinition(1))

	if _, err := handle.Enqueue(context.Background(), "not-a-uuid", testJobInput{}); err == nil || !strings.Contains(err.Error(), "canonical non-nil UUID") {
		t.Fatalf("invalid UUID error = %v", err)
	}
	large := testJobInput{Source: strings.Repeat("x", maxJobPayloadBytes)}
	if _, err := handle.Enqueue(context.Background(), testJobID, large); err == nil || !strings.Contains(err.Error(), "input exceeds 65536 bytes") {
		t.Fatalf("large input error = %v", err)
	}
	if requests := mock.Requests(); len(requests) != 0 {
		t.Fatalf("requests = %+v, want none", requests)
	}
	if err := handle.Cancel(context.Background(), "NOT-CANONICAL"); err == nil {
		t.Fatal("Cancel accepted an invalid UUID")
	}
	if _, err := handle.Get(context.Background(), "00000000-0000-0000-0000-000000000000"); err == nil {
		t.Fatal("Get accepted the nil UUID")
	}
}

func TestJobHandleGetTypedOutputAndCancel(t *testing.T) {
	a, mock := testAgent(t)
	handle := RegisterJob(a, testJobDefinition(1))
	job := testJobInfo(a, JobStatusSucceeded, json.RawMessage(`{"result":"converted"}`))
	now := time.Now().UTC().Truncate(time.Microsecond)
	job.AttemptCount = 2
	job.MaxAttempts = 3
	job.SourceRunID = "source-run"
	job.StartedAt = &now
	job.CompletedAt = &now
	job.AttemptLimit = 100
	job.Progress = &wire.JobProgress{Phase: "encoding", Message: "Halfway", Completed: 50, Total: 100}
	mock.GetJobResponse = &wire.GetJobResponse{Job: job}
	ctx := contextWithRun(context.Background(), newRun(a, "current-run", "", "", context.Background()))

	result, err := handle.Get(ctx, testJobID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Output == nil || result.Output.Result != "converted" || result.Created {
		t.Fatalf("result = %+v", result)
	}
	if result.AttemptCount != 2 || result.MaxAttempts != 3 || result.AttemptLimit != 100 || result.SourceRunID != "source-run" || result.CompletedAt == nil {
		t.Fatalf("metadata = %+v", result)
	}
	if result.Progress == nil || *result.Progress != (JobProgress{Phase: "encoding", Message: "Halfway", Completed: 50, Total: 100}) {
		t.Fatalf("progress = %+v", result.Progress)
	}
	if err := handle.Cancel(ctx, testJobID); err != nil {
		t.Fatal(err)
	}
	requests := mock.RequestsByPath("/api/agent/jobs/" + testJobID)
	if len(requests) != 2 || requests[0].Method != http.MethodGet || requests[1].Method != http.MethodDelete {
		t.Fatalf("requests = %+v", requests)
	}
	for _, request := range requests {
		if got := request.Header.Get("X-Airlock-Run-ID"); got != "current-run" {
			t.Fatalf("X-Airlock-Run-ID = %q", got)
		}
	}
}

func TestJobHandleGetDoesNotMaterializeLazyRun(t *testing.T) {
	a, mock := testAgent(t)
	handle := RegisterJob(a, testJobDefinition(1))
	mock.GetJobResponse = &wire.GetJobResponse{Job: testJobInfo(a, JobStatusQueued, json.RawMessage(`42`))}
	lazy := &lazyRun{agent: a, triggerRef: "GET /status"}

	result, err := handle.Get(contextWithLazyRun(context.Background(), lazy), testJobID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != JobStatusQueued || result.Output != nil {
		t.Fatalf("result = %+v", result)
	}
	if lazy.materialized() != nil {
		t.Fatal("Get materialized a lazy run")
	}
	if requests := mock.RequestsByPath("/api/agent/run/create"); len(requests) != 0 {
		t.Fatalf("run/create requests = %d, want 0", len(requests))
	}
	requests := mock.RequestsByPath("/api/agent/jobs/")
	if got := requests[0].Header.Get("X-Airlock-Run-ID"); got != "" {
		t.Fatalf("X-Airlock-Run-ID = %q, want empty", got)
	}
}

func TestJobHandleRejectsContractMismatchBeforeOutputDecode(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*wire.JobInfo)
	}{
		{name: "handler", mutate: func(job *wire.JobInfo) { job.HandlerName = "other" }},
		{name: "version", mutate: func(job *wire.JobInfo) { job.HandlerVersion++ }},
		{name: "output hash", mutate: func(job *wire.JobInfo) { job.OutputSchemaHash = "other" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, mock := testAgent(t)
			handle := RegisterJob(a, testJobDefinition(1))
			job := testJobInfo(a, JobStatusSucceeded, json.RawMessage(`42`))
			tt.mutate(&job)
			mock.GetJobResponse = &wire.GetJobResponse{Job: job}
			_, err := handle.Get(context.Background(), testJobID)
			if err == nil || !strings.Contains(err.Error(), "contract mismatch") {
				t.Fatalf("error = %v, want contract mismatch", err)
			}
			if strings.Contains(err.Error(), "decode") {
				t.Fatalf("output decoded before contract check: %v", err)
			}
		})
	}
}

func TestJobHandleRequiresSuccessfulOutput(t *testing.T) {
	a, mock := testAgent(t)
	handle := RegisterJob(a, testJobDefinition(1))
	mock.GetJobResponse = &wire.GetJobResponse{Job: testJobInfo(a, JobStatusSucceeded, nil)}
	if _, err := handle.Get(context.Background(), testJobID); err == nil || !strings.Contains(err.Error(), "has no output") {
		t.Fatalf("error = %v, want missing output", err)
	}
}

func TestJobHandleRejectsNilAndUnboundHandles(t *testing.T) {
	tests := []struct {
		name   string
		handle *JobHandle[testJobInput, testJobOutput]
	}{
		{name: "nil", handle: nil},
		{name: "unbound", handle: &JobHandle[testJobInput, testJobOutput]{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("Enqueue did not panic")
				}
			}()
			_, _ = tt.handle.Enqueue(context.Background(), testJobID, testJobInput{})
		})
	}
}

func TestRegisterJobLimits(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Job[testJobInput, testJobOutput])
		want   string
	}{
		{name: "description", mutate: func(job *Job[testJobInput, testJobOutput]) { job.Description = strings.Repeat("x", 4097) }, want: "Description exceeds 4096 bytes"},
		{name: "timeout", mutate: func(job *Job[testJobInput, testJobOutput]) { job.Timeout = 24*time.Hour + time.Millisecond }, want: "Timeout exceeds 24 hours"},
		{name: "attempts", mutate: func(job *Job[testJobInput, testJobOutput]) { job.MaxAttempts = 101 }, want: "MaxAttempts exceeds 100"},
		{name: "concurrency", mutate: func(job *Job[testJobInput, testJobOutput]) { job.MaxConcurrency = 1001 }, want: "MaxConcurrency exceeds 1000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, _ := testAgent(t)
			job := testJobDefinition(1)
			tt.mutate(job)
			defer func() {
				got := recover()
				if got == nil || !strings.Contains(got.(string), tt.want) {
					t.Fatalf("panic = %v, want %q", got, tt.want)
				}
			}()
			RegisterJob(a, job)
		})
	}
}

func TestRegisterJobSchemaLimits(t *testing.T) {
	a, _ := testAgent(t)
	RegisterJob(a, testJobDefinition(1))
	registered := *a.jobs[jobKey{name: "convert_video", version: 1}]
	tests := []struct {
		name   string
		mutate func(*registeredJob)
		want   string
	}{
		{name: "input", mutate: func(job *registeredJob) { job.inputSchema = json.RawMessage(strings.Repeat(" ", maxJobSchemaBytes+1)) }, want: "InputSchema exceeds 256 KiB"},
		{name: "output", mutate: func(job *registeredJob) { job.outputSchema = json.RawMessage(strings.Repeat(" ", maxJobSchemaBytes+1)) }, want: "OutputSchema exceeds 256 KiB"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := registered
			tt.mutate(&job)
			defer func() {
				got := recover()
				if got == nil || !strings.Contains(got.(string), tt.want) {
					t.Fatalf("panic = %v, want %q", got, tt.want)
				}
			}()
			validateRegisteredJob(&job)
		})
	}
}

func TestRegisterJobAcceptsLimitBoundaries(t *testing.T) {
	a, _ := testAgent(t)
	job := testJobDefinition(1)
	job.Description = strings.Repeat("x", maxJobDescriptionBytes)
	job.Timeout = maxJobTimeout
	job.MaxAttempts = maxJobAttempts
	job.MaxConcurrency = maxJobConcurrency
	RegisterJob(a, job)
}

func TestHandleJobSuccessAndCallerContext(t *testing.T) {
	a, mock := testAgent(t)
	definition := testJobDefinition(1)
	var gotJob JobContext
	definition.Handler = func(ctx context.Context, job JobContext, input testJobInput) (testJobOutput, error) {
		gotJob = job
		if AgentFromContext(ctx) != a {
			t.Error("AgentFromContext did not return the dispatching agent")
		}
		user, ok := UserFromContext(ctx)
		if !ok || user.ID != testJobUserID {
			t.Errorf("UserFromContext = %+v, %t", user, ok)
		}
		caller := callerFromContext(ctx)
		if caller.Access != AccessUser || caller.UserID != testJobUserID {
			t.Errorf("caller = %+v", caller)
		}
		if got := runFromContext(ctx).conversationID; got != testJobConversation {
			t.Errorf("conversation ID = %q", got)
		}
		return testJobOutput{Result: "converted:" + input.Source}, nil
	}
	RegisterJob(a, definition)

	request := validJobRunRequest(a)
	scheduledAt := time.Date(2026, time.August, 21, 9, 30, 0, 0, time.UTC)
	request.ScheduledAt = &scheduledAt
	w := serveJobRequest(t, a, request, testJobRunID)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var response wire.JobRunResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Status != "success" || string(response.Output) != `{"result":"converted:uploads/video.mov"}` || response.Error != "" {
		t.Fatalf("response = %+v", response)
	}
	if gotJob.ID != testJobID || gotJob.Attempt != 2 || gotJob.ScheduledAt == nil || !gotJob.ScheduledAt.Equal(scheduledAt) {
		t.Fatalf("job context = %+v", gotJob)
	}
	if got := completedRun(t, mock); got.Status != "success" {
		t.Fatalf("completion = %+v", got)
	}
}

func TestJobContextReportProgress(t *testing.T) {
	a, mock := testAgent(t)
	definition := testJobDefinition(1)
	definition.Handler = func(ctx context.Context, job JobContext, _ testJobInput) (testJobOutput, error) {
		if err := job.ReportProgress(ctx, JobProgress{Phase: "encoding", Message: "Frame 40 of 100", Completed: 40, Total: 100}); err != nil {
			return testJobOutput{}, err
		}
		return testJobOutput{Result: "converted"}, nil
	}
	RegisterJob(a, definition)

	w := serveJobRequestWithLeaseToken(t, a, validJobRunRequest(a), testJobRunID, testJobLeaseToken)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	requests := mock.RequestsByPath("/api/agent/jobs/" + testJobID + "/progress")
	if len(requests) != 1 {
		t.Fatalf("progress requests = %d, want 1", len(requests))
	}
	request := requests[0]
	if request.Method != http.MethodPut || request.Path != "/api/agent/jobs/"+testJobID+"/progress" {
		t.Fatalf("request = %s %s", request.Method, request.Path)
	}
	if got := request.Header.Get("Authorization"); got != "Bearer test-token" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := request.Header.Get("X-Airlock-Run-ID"); got != testJobRunID {
		t.Fatalf("X-Airlock-Run-ID = %q", got)
	}
	if got := request.Header.Get(jobLeaseTokenHeader); got != testJobLeaseToken {
		t.Fatalf("%s = %q", jobLeaseTokenHeader, got)
	}
	if got := request.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	var body wire.UpdateJobProgressRequest
	if err := json.Unmarshal(request.Body, &body); err != nil {
		t.Fatal(err)
	}
	want := wire.UpdateJobProgressRequest{Attempt: 2, Phase: "encoding", Message: "Frame 40 of 100", Completed: 40, Total: 100}
	if body != want {
		t.Fatalf("body = %+v, want %+v", body, want)
	}
}

func TestJobContextReportProgressRequiresMatchingDelivery(t *testing.T) {
	a, mock := testAgent(t)
	validJob, validContext := boundJobProgressContext(a, testJobLeaseToken)
	tests := []struct {
		name string
		job  JobContext
		ctx  context.Context
		want string
	}{
		{name: "unbound", job: validJob, ctx: context.Background(), want: "not bound to an active job run"},
		{name: "job ID mismatch", job: JobContext{ID: "66666666-6666-6666-6666-666666666666", Attempt: validJob.Attempt}, ctx: validContext, want: "does not match"},
		{name: "attempt mismatch", job: JobContext{ID: validJob.ID, Attempt: validJob.Attempt + 1}, ctx: validContext, want: "does not match"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.job.ReportProgress(tt.ctx, JobProgress{Phase: "working"})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
	if requests := mock.RequestsByPath("/api/agent/jobs/"); len(requests) != 0 {
		t.Fatalf("job requests = %+v, want none", requests)
	}
}

func TestJobContextReportProgressRequiresLeaseToken(t *testing.T) {
	a, mock := testAgent(t)
	var reportErr error
	definition := testJobDefinition(1)
	definition.Handler = func(ctx context.Context, job JobContext, _ testJobInput) (testJobOutput, error) {
		reportErr = job.ReportProgress(ctx, JobProgress{Phase: "working"})
		return testJobOutput{Result: "completed without progress"}, nil
	}
	RegisterJob(a, definition)

	w := serveJobRequest(t, a, validJobRunRequest(a), testJobRunID)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if reportErr == nil || !strings.Contains(reportErr.Error(), "did not include a job lease token") {
		t.Fatalf("error = %v, want missing lease token", reportErr)
	}
	if requests := mock.RequestsByPath("/api/agent/jobs/"); len(requests) != 0 {
		t.Fatalf("job requests = %+v, want none", requests)
	}
}

func TestJobContextReportProgressValidation(t *testing.T) {
	tests := []struct {
		name     string
		progress JobProgress
		want     string
	}{
		{name: "blank phase", progress: JobProgress{Phase: " \t\n"}, want: "Phase must not be blank"},
		{name: "phase too long", progress: JobProgress{Phase: strings.Repeat("x", maxJobProgressPhaseBytes+1)}, want: "Phase exceeds 128 bytes"},
		{name: "message too long", progress: JobProgress{Phase: "working", Message: strings.Repeat("x", maxJobProgressMessageBytes+1)}, want: "Message exceeds 4096 bytes"},
		{name: "negative completed", progress: JobProgress{Phase: "working", Completed: -1, Total: 1}, want: "Completed must be nonnegative"},
		{name: "negative total", progress: JobProgress{Phase: "working", Total: -1}, want: "Total must be nonnegative"},
		{name: "completed with zero total", progress: JobProgress{Phase: "working", Completed: 1}, want: "Completed must be zero when Total is zero"},
		{name: "completed above total", progress: JobProgress{Phase: "working", Completed: 2, Total: 1}, want: "Completed must not exceed Total"},
	}
	a, mock := testAgent(t)
	job, ctx := boundJobProgressContext(a, testJobLeaseToken)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := job.ReportProgress(ctx, tt.progress)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
	if requests := mock.RequestsByPath("/api/agent/jobs/"); len(requests) != 0 {
		t.Fatalf("job requests = %+v, want none", requests)
	}
}

func TestJobContextReportProgressAcceptsValidationBoundaries(t *testing.T) {
	a, mock := testAgent(t)
	job, ctx := boundJobProgressContext(a, testJobLeaseToken)
	progress := JobProgress{
		Phase:     strings.Repeat("p", maxJobProgressPhaseBytes),
		Message:   strings.Repeat("m", maxJobProgressMessageBytes),
		Completed: 100,
		Total:     100,
	}
	if err := job.ReportProgress(ctx, progress); err != nil {
		t.Fatal(err)
	}
	if requests := mock.RequestsByPath("/api/agent/jobs/" + testJobID + "/progress"); len(requests) != 1 {
		t.Fatalf("progress requests = %d, want 1", len(requests))
	}
}

func TestJobContextReportProgressReturnsServerRejection(t *testing.T) {
	a, mock := testAgent(t)
	mock.JobProgressStatus = http.StatusConflict
	job, ctx := boundJobProgressContext(a, testJobLeaseToken)

	err := job.ReportProgress(ctx, JobProgress{Phase: "working", Completed: 1, Total: 2})
	if err == nil || !strings.Contains(err.Error(), "status 409") || !strings.Contains(err.Error(), "job progress rejected") {
		t.Fatalf("error = %v, want server rejection", err)
	}
	if requests := mock.RequestsByPath("/api/agent/jobs/" + testJobID + "/progress"); len(requests) != 1 {
		t.Fatalf("progress requests = %d, want 1", len(requests))
	}
}

func TestHandleJobRequiresDurableRunCompletion(t *testing.T) {
	a, mock := testAgent(t)
	mock.RunCompleteStatus = http.StatusConflict
	RegisterJob(a, testJobDefinition(1))

	w := serveJobRequest(t, a, validJobRunRequest(a), testJobRunID)
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "record job run completion") {
		t.Fatalf("response = %d %q", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"status":"success"`) {
		t.Fatalf("response exposed success after rejected completion: %s", w.Body.String())
	}
}

func TestHandleJobRejectsUnknownAndContractMismatch(t *testing.T) {
	t.Run("unknown", func(t *testing.T) {
		a, mock := testAgent(t)
		RegisterJob(a, testJobDefinition(1))
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/job/other/1", nil)
		a.Handler().ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", w.Code)
		}
		if got := mock.RequestsByPath("/api/agent/run/complete"); len(got) != 0 {
			t.Fatalf("completion requests = %d, want 0", len(got))
		}
	})

	t.Run("contract mismatch", func(t *testing.T) {
		a, mock := testAgent(t)
		executed := false
		definition := testJobDefinition(1)
		definition.Handler = func(context.Context, JobContext, testJobInput) (testJobOutput, error) {
			executed = true
			return testJobOutput{}, nil
		}
		RegisterJob(a, definition)
		request := validJobRunRequest(a)
		request.OutputSchemaHash = strings.Repeat("0", 64)
		w := serveJobRequest(t, a, request, testJobRunID)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "contract mismatch") {
			t.Fatalf("response = %d %q", w.Code, w.Body.String())
		}
		if executed {
			t.Fatal("handler ran for a mismatched contract")
		}
		if got := mock.RequestsByPath("/api/agent/run/complete"); len(got) != 0 {
			t.Fatalf("completion requests = %d, want 0", len(got))
		}
	})
}

func TestHandleJobTerminalFailures(t *testing.T) {
	tests := []struct {
		name       string
		timeout    time.Duration
		handler    JobHandlerFunc[testJobInput, testJobOutput]
		wantStatus string
		wantError  string
		wantTrace  bool
	}{
		{
			name: "handler error",
			handler: func(context.Context, JobContext, testJobInput) (testJobOutput, error) {
				return testJobOutput{}, errors.New("retry me")
			},
			wantStatus: "error", wantError: "retry me",
		},
		{
			name: "timeout", timeout: 5 * time.Millisecond,
			handler: func(ctx context.Context, _ JobContext, _ testJobInput) (testJobOutput, error) {
				<-ctx.Done()
				return testJobOutput{}, ctx.Err()
			},
			wantStatus: "timeout", wantError: context.DeadlineExceeded.Error(),
		},
		{
			name: "panic",
			handler: func(context.Context, JobContext, testJobInput) (testJobOutput, error) {
				panic("job panic")
			},
			wantStatus: "error", wantError: "job panic", wantTrace: true,
		},
		{
			name: "oversized output",
			handler: func(context.Context, JobContext, testJobInput) (testJobOutput, error) {
				return testJobOutput{Result: strings.Repeat("x", maxJobPayloadBytes)}, nil
			},
			wantStatus: "error", wantError: "job output exceeds 65536 bytes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, mock := testAgent(t)
			definition := testJobDefinition(1)
			definition.Handler = tt.handler
			if tt.timeout != 0 {
				definition.Timeout = tt.timeout
			}
			RegisterJob(a, definition)
			w := serveJobRequest(t, a, validJobRunRequest(a), testJobRunID)
			var response wire.JobRunResponse
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.Status != tt.wantStatus || response.Error != tt.wantError || len(response.Output) != 0 {
				t.Fatalf("response = %+v", response)
			}
			completion := completedRun(t, mock)
			if completion.Status != tt.wantStatus || completion.Error != tt.wantError || (completion.PanicTrace != "") != tt.wantTrace {
				t.Fatalf("completion = %+v", completion)
			}
		})
	}
}

func TestHandleJobWrappedEnqueueUnavailableIsRetryable(t *testing.T) {
	a, mock := testAgent(t)
	definition := testJobDefinition(1)
	unavailable := &JobEnqueueUnavailableError{
		HandlerName:    "follow_up",
		HandlerVersion: 3,
		Message:        "deployment transition",
	}
	definition.Handler = func(context.Context, JobContext, testJobInput) (testJobOutput, error) {
		return testJobOutput{}, fmt.Errorf("enqueue follow-up: %w", unavailable)
	}
	RegisterJob(a, definition)

	w := serveJobRequest(t, a, validJobRunRequest(a), testJobRunID)
	var response wire.JobRunResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Status != "retry" || !strings.Contains(response.Error, "enqueue follow-up") || len(response.Output) != 0 {
		t.Fatalf("response = %+v", response)
	}
	completion := completedRun(t, mock)
	if completion.Status != "error" || completion.ErrorKind != wire.ErrorKindPlatform || completion.Error != response.Error {
		t.Fatalf("completion = %+v", completion)
	}
}

func TestHandleJobCancellation(t *testing.T) {
	a, mock := testAgent(t)
	definition := testJobDefinition(1)
	definition.Handler = func(ctx context.Context, _ JobContext, _ testJobInput) (testJobOutput, error) {
		return testJobOutput{}, ctx.Err()
	}
	RegisterJob(a, definition)
	request := validJobRunRequest(a)
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest(http.MethodPost, "/job/convert_video/1", strings.NewReader(string(body))).WithContext(ctx)
	r.Header.Set("X-Run-ID", testJobRunID)
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	var response wire.JobRunResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Status != "error" || response.Error != context.Canceled.Error() {
		t.Fatalf("response = %+v", response)
	}
	if got := completedRun(t, mock); got.Status != "error" || got.Error != context.Canceled.Error() {
		t.Fatalf("completion = %+v", got)
	}
}

func TestHandleJobRequiresRunID(t *testing.T) {
	a, mock := testAgent(t)
	RegisterJob(a, testJobDefinition(1))
	defer func() {
		got := recover()
		if got != "agentsdk: X-Run-ID header is required" {
			t.Fatalf("panic = %v", got)
		}
		if requests := mock.RequestsByPath("/api/agent/run/complete"); len(requests) != 0 {
			t.Fatalf("completion requests = %d, want 0", len(requests))
		}
	}()
	serveJobRequest(t, a, validJobRunRequest(a), "")
}

func TestHandleJobStrictBoundedRequest(t *testing.T) {
	tests := []struct {
		name string
		body func(*Agent) string
	}{
		{
			name: "unknown field",
			body: func(a *Agent) string {
				body, _ := json.Marshal(validJobRunRequest(a))
				return strings.TrimSuffix(string(body), "}") + `,"unknown":true}`
			},
		},
		{
			name: "trailing value",
			body: func(a *Agent) string {
				body, _ := json.Marshal(validJobRunRequest(a))
				return string(body) + `{}`
			},
		},
		{
			name: "oversized input",
			body: func(a *Agent) string {
				request := validJobRunRequest(a)
				request.Input = json.RawMessage(`{"source":"` + strings.Repeat("x", maxJobPayloadBytes) + `"}`)
				body, _ := json.Marshal(request)
				return string(body)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, mock := testAgent(t)
			RegisterJob(a, testJobDefinition(1))
			r := httptest.NewRequest(http.MethodPost, "/job/convert_video/1", strings.NewReader(tt.body(a)))
			r.Header.Set("X-Run-ID", testJobRunID)
			w := httptest.NewRecorder()
			a.Handler().ServeHTTP(w, r)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
			}
			if got := mock.RequestsByPath("/api/agent/run/complete"); len(got) != 0 {
				t.Fatalf("completion requests = %d, want 0", len(got))
			}
		})
	}
}

func TestHandleJobCanonicalDeliveryValidation(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		mutate func(*wire.JobRunRequest)
	}{
		{name: "noncanonical job ID", mutate: func(request *wire.JobRunRequest) { request.ID = "AAAAAAAA-AAAA-AAAA-AAAA-AAAAAAAAAAAA" }},
		{name: "zero attempt", mutate: func(request *wire.JobRunRequest) { request.Attempt = 0 }},
		{name: "timeout mismatch", mutate: func(request *wire.JobRunRequest) { request.TimeoutMs-- }},
		{name: "zero scheduled time", mutate: func(request *wire.JobRunRequest) { request.ScheduledAt = new(time.Time) }},
		{name: "noncanonical version path", path: "/job/convert_video/01"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, mock := testAgent(t)
			RegisterJob(a, testJobDefinition(1))
			request := validJobRunRequest(a)
			if tt.mutate != nil {
				tt.mutate(&request)
			}
			body, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			path := tt.path
			if path == "" {
				path = "/job/convert_video/1"
			}
			r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
			r.Header.Set("X-Run-ID", testJobRunID)
			w := httptest.NewRecorder()
			a.Handler().ServeHTTP(w, r)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
			}
			if got := mock.RequestsByPath("/api/agent/run/complete"); len(got) != 0 {
				t.Fatalf("completion requests = %d, want 0", len(got))
			}
		})
	}
}

func TestHandleJobAcceptsReplacementAttemptAboveContractLimit(t *testing.T) {
	a, _ := testAgent(t)
	definition := testJobDefinition(1)
	var gotAttempt int
	definition.Handler = func(_ context.Context, job JobContext, _ testJobInput) (testJobOutput, error) {
		gotAttempt = job.Attempt
		return testJobOutput{Result: "retried"}, nil
	}
	RegisterJob(a, definition)
	request := validJobRunRequest(a)
	request.Attempt = maxJobAttempts + 1

	w := serveJobRequest(t, a, request, testJobRunID)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if gotAttempt != maxJobAttempts+1 {
		t.Fatalf("attempt = %d, want %d", gotAttempt, maxJobAttempts+1)
	}
}

func TestHandleJobRejectsInvalidLeaseToken(t *testing.T) {
	for _, token := range []string{
		"AAAAAAAA-AAAA-AAAA-AAAA-AAAAAAAAAAAA",
		"00000000-0000-0000-0000-000000000000",
	} {
		t.Run(token, func(t *testing.T) {
			a, mock := testAgent(t)
			executed := false
			definition := testJobDefinition(1)
			definition.Handler = func(context.Context, JobContext, testJobInput) (testJobOutput, error) {
				executed = true
				return testJobOutput{}, nil
			}
			RegisterJob(a, definition)

			w := serveJobRequestWithLeaseToken(t, a, validJobRunRequest(a), testJobRunID, token)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid job lease token header") {
				t.Fatalf("response = %d %q", w.Code, w.Body.String())
			}
			if executed {
				t.Fatal("handler ran with an invalid lease token")
			}
			if got := mock.RequestsByPath("/api/agent/run/complete"); len(got) != 0 {
				t.Fatalf("completion requests = %d, want 0", len(got))
			}
		})
	}
}

func serveJobRequest(t *testing.T, a *Agent, request wire.JobRunRequest, runID string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/job/convert_video/1", strings.NewReader(string(body)))
	if runID != "" {
		r.Header.Set("X-Run-ID", runID)
	}
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	return w
}

func serveJobRequestWithLeaseToken(t *testing.T, a *Agent, request wire.JobRunRequest, runID, leaseToken string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/job/convert_video/1", strings.NewReader(string(body)))
	r.Header.Set("X-Run-ID", runID)
	r.Header.Set(jobLeaseTokenHeader, leaseToken)
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	return w
}

func boundJobProgressContext(a *Agent, leaseToken string) (JobContext, context.Context) {
	job := JobContext{ID: testJobID, Attempt: 2}
	run := newRun(a, testJobRunID, "", "", context.Background())
	ctx := contextWithRun(context.Background(), run)
	ctx = contextWithJobRun(ctx, &jobRunContext{agent: a, id: job.ID, attempt: job.Attempt, leaseToken: leaseToken})
	return job, ctx
}

func validJobRunRequest(a *Agent) wire.JobRunRequest {
	job := a.jobs[jobKey{name: "convert_video", version: 1}]
	return wire.JobRunRequest{
		ID: testJobID, Name: job.name, Version: int32(job.version),
		InputSchemaHash: job.inputSchemaHash, OutputSchemaHash: job.outputSchemaHash,
		Attempt: 2, TimeoutMs: job.timeout.Milliseconds(), Input: json.RawMessage(`{"source":"uploads/video.mov"}`),
		InitiatorKind: "user", InitiatorUserID: testJobUserID, InitiatorConversationID: testJobConversation,
		CallerAccess: wire.AccessUser,
	}
}

func testJobInfo(a *Agent, status JobStatus, output json.RawMessage) wire.JobInfo {
	job := a.jobs[jobKey{name: "convert_video", version: 1}]
	now := time.Date(2026, time.August, 21, 12, 0, 0, 0, time.UTC)
	return wire.JobInfo{
		ID: testJobID, AgentID: a.agentID, HandlerName: job.name, HandlerVersion: int32(job.version),
		InputSchemaHash: job.inputSchemaHash, OutputSchemaHash: job.outputSchemaHash,
		Status: string(status), Input: json.RawMessage(`{"source":"uploads/video.mov"}`), Output: output,
		MaxAttempts: int32(job.maxAttempts), CreatedAt: now, UpdatedAt: now,
	}
}
