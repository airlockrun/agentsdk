package agentsdk

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/airlockrun/agentsdk/wire"
	"github.com/airlockrun/goai/schema"
	"github.com/google/uuid"
)

const (
	maxJobPayloadBytes         = 64 * 1024
	maxJobRunRequestBytes      = maxJobPayloadBytes + 16*1024
	maxJobProgressPhaseBytes   = 128
	maxJobProgressMessageBytes = 4096
	jobLeaseTokenHeader        = "X-Airlock-Job-Lease-Token"
)

type jobRunContext struct {
	agent      *Agent
	id         string
	attempt    int
	leaseToken string
}

type jobRunContextKey struct{}

func contextWithJobRun(ctx context.Context, job *jobRunContext) context.Context {
	return context.WithValue(ctx, jobRunContextKey{}, job)
}

func jobRunFromContext(ctx context.Context) *jobRunContext {
	if ctx == nil {
		return nil
	}
	job, _ := ctx.Value(jobRunContextKey{}).(*jobRunContext)
	return job
}

// JobContext identifies one delivery attempt for a durable background job.
// Handlers use ID as the idempotency key because delivery is at least once.
type JobContext struct {
	ID          string
	Attempt     int
	ScheduledAt *time.Time
}

// JobProgress is the latest durable progress reported by a job attempt.
type JobProgress struct {
	Phase     string
	Message   string
	Completed int64
	Total     int64
}

// ReportProgress synchronously records progress for this delivery attempt.
func (job JobContext) ReportProgress(ctx context.Context, progress JobProgress) error {
	active := jobRunFromContext(ctx)
	if active == nil {
		return errors.New("agentsdk: JobContext.ReportProgress: context is not bound to an active job run")
	}
	if job.ID != active.id || job.Attempt != active.attempt {
		return errors.New("agentsdk: JobContext.ReportProgress: JobContext does not match the active job run")
	}
	if active.leaseToken == "" {
		return errors.New("agentsdk: JobContext.ReportProgress: delivery did not include a job lease token")
	}
	if err := validateJobProgress(progress); err != nil {
		return fmt.Errorf("agentsdk: JobContext.ReportProgress: %w", err)
	}
	request := wire.UpdateJobProgressRequest{
		Attempt:   int32(job.Attempt),
		Phase:     progress.Phase,
		Message:   progress.Message,
		Completed: progress.Completed,
		Total:     progress.Total,
	}
	headers := make(http.Header)
	headers.Set(jobLeaseTokenHeader, active.leaseToken)
	return active.agent.client.doJSONWithHeaders(ctx, http.MethodPut, "/api/agent/jobs/"+job.ID+"/progress", request, nil, headers)
}

func validateJobProgress(progress JobProgress) error {
	if strings.TrimSpace(progress.Phase) == "" {
		return errors.New("Phase must not be blank")
	}
	if len(progress.Phase) > maxJobProgressPhaseBytes {
		return errors.New("Phase exceeds 128 bytes")
	}
	if len(progress.Message) > maxJobProgressMessageBytes {
		return errors.New("Message exceeds 4096 bytes")
	}
	if progress.Completed < 0 {
		return errors.New("Completed must be nonnegative")
	}
	if progress.Total < 0 {
		return errors.New("Total must be nonnegative")
	}
	if progress.Total == 0 && progress.Completed != 0 {
		return errors.New("Completed must be zero when Total is zero")
	}
	if progress.Total > 0 && progress.Completed > progress.Total {
		return errors.New("Completed must not exceed Total")
	}
	return nil
}

// JobHandlerFunc handles one typed background-job delivery attempt.
type JobHandlerFunc[In, Out any] func(ctx context.Context, job JobContext, input In) (Out, error)

// Job declares one versioned background-job contract.
type Job[In, Out any] struct {
	noUnkeyedLiterals

	Name           string                  // lowercase snake_case, unique with Version
	Version        int                     // positive immutable contract version
	Description    string                  // required: shown to operators
	Timeout        time.Duration           // required maximum execution time
	MaxAttempts    int                     // required, including the first attempt
	MaxConcurrency int                     // required per-agent handler concurrency
	Handler        JobHandlerFunc[In, Out] // required
}

// JobCron declares a recurring enqueue of one registered job contract.
type JobCron[In any] struct {
	noUnkeyedLiterals

	Slug        string // lowercase snake_case, unique across the agent
	Schedule    string // standard cron expression, e.g. "0 9 * * *"
	Input       In     // static input included with every enqueue
	Description string // required: shown to operators
}

// JobHandle binds a typed job contract to an agent. Enqueue operations are
// added to this handle with the durable job lifecycle API.
type JobHandle[In, Out any] struct {
	agent   *Agent
	name    string
	version int
}

// ErrJobEnqueueUnavailable identifies a temporary deployment-state rejection
// of an exact job handler contract.
var ErrJobEnqueueUnavailable = errors.New("agentsdk: job enqueue unavailable")

// JobEnqueueUnavailableError reports the exact handler contract Airlock could
// not accept during a deployment transition.
type JobEnqueueUnavailableError struct {
	HandlerName    string
	HandlerVersion int
	Message        string
}

func (e *JobEnqueueUnavailableError) Error() string {
	return fmt.Sprintf("%s for %s@v%d: %s", ErrJobEnqueueUnavailable, e.HandlerName, e.HandlerVersion, e.Message)
}

func (e *JobEnqueueUnavailableError) Unwrap() error { return ErrJobEnqueueUnavailable }

// JobStatus is the durable lifecycle state of a background job.
type JobStatus string

const (
	JobStatusQueued    JobStatus = "queued"
	JobStatusRunning   JobStatus = "running"
	JobStatusSucceeded JobStatus = "succeeded"
	JobStatusFailed    JobStatus = "failed"
	JobStatusCancelled JobStatus = "cancelled"
)

// JobResult reports durable lifecycle state and the typed output of a
// successful job. Created is true only when Enqueue accepted a new job rather
// than returning the existing job for the same ID.
type JobResult[Out any] struct {
	ID           string
	Status       JobStatus
	AttemptCount int
	MaxAttempts  int
	AttemptLimit int
	LastError    string
	Progress     *JobProgress
	SourceRunID  string
	ScheduledAt  *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
	StartedAt    *time.Time
	CompletedAt  *time.Time
	Output       *Out
	Created      bool
}

// Name returns the registered handler name.
func (h *JobHandle[In, Out]) Name() string { return h.name }

// Version returns the registered handler contract version.
func (h *JobHandle[In, Out]) Version() int { return h.version }

// Cron registers a recurring enqueue targeting this exact job contract.
func (h *JobHandle[In, Out]) Cron(cron *JobCron[In]) {
	if h == nil {
		panic("agentsdk: nil *JobHandle")
	}
	if h.agent == nil {
		panic("agentsdk: unbound JobHandle")
	}
	done := h.agent.beginRegistration("JobHandle.Cron")
	defer done()
	job := h.registeredJob()
	if cron == nil {
		panic(fmt.Sprintf("agentsdk: JobHandle(%s@v%d).Cron: nil *JobCron", job.name, job.version))
	}
	context := fmt.Sprintf("JobHandle(%s@v%d).Cron", job.name, job.version)
	validateLocalIdentifier(context, "Slug", cron.Slug, maxLocalSlugLength)
	if _, err := cronParser.Parse(cron.Schedule); err != nil {
		panic(fmt.Sprintf("agentsdk: %s(%q): invalid Schedule: %v", context, cron.Slug, err))
	}
	if strings.TrimSpace(cron.Description) == "" {
		panic(fmt.Sprintf("agentsdk: %s(%q): Description is required", context, cron.Slug))
	}
	if len(cron.Description) > maxJobDescriptionBytes {
		panic(fmt.Sprintf("agentsdk: %s(%q): Description exceeds 4096 bytes", context, cron.Slug))
	}
	input, err := json.Marshal(cron.Input)
	if err != nil {
		panic(fmt.Sprintf("agentsdk: %s(%q): encode Input: %v", context, cron.Slug, err))
	}
	if len(input) > maxJobPayloadBytes {
		panic(fmt.Sprintf("agentsdk: %s(%q): Input exceeds %d bytes", context, cron.Slug, maxJobPayloadBytes))
	}
	if _, exists := h.agent.jobCrons[cron.Slug]; exists {
		panic("agentsdk: duplicate job cron slug: " + cron.Slug)
	}
	h.agent.jobCrons[cron.Slug] = &registeredJobCron{
		slug:             cron.Slug,
		schedule:         cron.Schedule,
		description:      cron.Description,
		handlerName:      job.name,
		handlerVersion:   job.version,
		inputSchemaHash:  job.inputSchemaHash,
		outputSchemaHash: job.outputSchemaHash,
		input:            input,
	}
}

// Enqueue durably accepts one caller-identified job. Retrying the same ID while
// Airlock retains the job is idempotent and returns the existing job.
func (h *JobHandle[In, Out]) Enqueue(ctx context.Context, id string, input In) (JobResult[Out], error) {
	return h.enqueue(ctx, id, nil, input, "Enqueue")
}

// EnqueueAt durably accepts one caller-identified job for delivery at fireAt.
// Retrying the same ID while Airlock retains the job is idempotent and returns
// the existing job.
func (h *JobHandle[In, Out]) EnqueueAt(ctx context.Context, id string, fireAt time.Time, input In) (JobResult[Out], error) {
	job := h.registeredJob()
	if fireAt.IsZero() {
		return JobResult[Out]{}, fmt.Errorf("agentsdk: JobHandle(%s@v%d).EnqueueAt: fireAt is required", job.name, job.version)
	}
	scheduledAt := fireAt.UTC().Truncate(time.Microsecond)
	return h.enqueue(ctx, id, &scheduledAt, input, "EnqueueAt")
}

func (h *JobHandle[In, Out]) enqueue(ctx context.Context, id string, scheduledAt *time.Time, input In, operation string) (JobResult[Out], error) {
	job := h.registeredJob()
	if err := validateJobID(id); err != nil {
		return JobResult[Out]{}, fmt.Errorf("agentsdk: JobHandle(%s@v%d).%s: %w", job.name, job.version, operation, err)
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return JobResult[Out]{}, fmt.Errorf("agentsdk: JobHandle(%s@v%d).%s: encode input: %w", job.name, job.version, operation, err)
	}
	if len(encoded) > maxJobPayloadBytes {
		return JobResult[Out]{}, fmt.Errorf("agentsdk: JobHandle(%s@v%d).%s: input exceeds %d bytes", job.name, job.version, operation, maxJobPayloadBytes)
	}

	run := h.agent.runForCall(ctx)
	ctx = contextWithRun(ctx, run)
	request := wire.EnqueueJobRequest{
		ID:               id,
		Name:             job.name,
		Version:          int32(job.version),
		InputSchemaHash:  job.inputSchemaHash,
		OutputSchemaHash: job.outputSchemaHash,
		Input:            encoded,
		ScheduledAt:      scheduledAt,
	}
	var response wire.EnqueueJobResponse
	if err := h.agent.client.doJSON(ctx, "POST", "/api/agent/jobs", request, &response); err != nil {
		return JobResult[Out]{}, err
	}
	return jobResult[Out](job, response.Job, response.Created)
}

// Get returns the current durable state of a job.
func (h *JobHandle[In, Out]) Get(ctx context.Context, id string) (JobResult[Out], error) {
	job := h.registeredJob()
	if err := validateJobID(id); err != nil {
		return JobResult[Out]{}, fmt.Errorf("agentsdk: JobHandle(%s@v%d).Get: %w", job.name, job.version, err)
	}
	var response wire.GetJobResponse
	if err := h.agent.client.doJSON(ctx, "GET", "/api/agent/jobs/"+id, nil, &response); err != nil {
		return JobResult[Out]{}, err
	}
	return jobResult[Out](job, response.Job, false)
}

// Cancel requests cancellation of a queued or running job.
func (h *JobHandle[In, Out]) Cancel(ctx context.Context, id string) error {
	job := h.registeredJob()
	if err := validateJobID(id); err != nil {
		return fmt.Errorf("agentsdk: JobHandle(%s@v%d).Cancel: %w", job.name, job.version, err)
	}
	return h.agent.client.doJSON(ctx, "DELETE", "/api/agent/jobs/"+id, nil, nil)
}

func (h *JobHandle[In, Out]) registeredJob() *registeredJob {
	if h == nil {
		panic("agentsdk: nil *JobHandle")
	}
	if h.agent == nil {
		panic("agentsdk: unbound JobHandle")
	}
	job := h.agent.jobs[jobKey{name: h.name, version: h.version}]
	if job == nil {
		panic(fmt.Sprintf("agentsdk: unbound JobHandle %s@v%d", h.name, h.version))
	}
	return job
}

func validateJobID(id string) error {
	parsed, err := uuid.Parse(id)
	if err != nil || parsed == uuid.Nil || parsed.String() != id {
		return errors.New("ID must be a canonical non-nil UUID")
	}
	return nil
}

func jobResult[Out any](contract *registeredJob, job wire.JobInfo, created bool) (JobResult[Out], error) {
	if job.HandlerName != contract.name {
		return JobResult[Out]{}, fmt.Errorf("agentsdk: job %s contract mismatch: handler is %q, want %q", job.ID, job.HandlerName, contract.name)
	}
	if job.HandlerVersion != int32(contract.version) {
		return JobResult[Out]{}, fmt.Errorf("agentsdk: job %s contract mismatch: handler version is %d, want %d", job.ID, job.HandlerVersion, contract.version)
	}
	if job.InputSchemaHash != contract.inputSchemaHash {
		return JobResult[Out]{}, fmt.Errorf("agentsdk: job %s contract mismatch: input schema hash is %q, want %q", job.ID, job.InputSchemaHash, contract.inputSchemaHash)
	}
	if job.OutputSchemaHash != contract.outputSchemaHash {
		return JobResult[Out]{}, fmt.Errorf("agentsdk: job %s contract mismatch: output schema hash is %q, want %q", job.ID, job.OutputSchemaHash, contract.outputSchemaHash)
	}
	result := JobResult[Out]{
		ID:           job.ID,
		Status:       JobStatus(job.Status),
		AttemptCount: int(job.AttemptCount),
		MaxAttempts:  int(job.MaxAttempts),
		AttemptLimit: int(job.AttemptLimit),
		LastError:    job.LastError,
		SourceRunID:  job.SourceRunID,
		ScheduledAt:  job.ScheduledAt,
		CreatedAt:    job.CreatedAt,
		UpdatedAt:    job.UpdatedAt,
		StartedAt:    job.StartedAt,
		CompletedAt:  job.CompletedAt,
		Created:      created,
	}
	if job.Progress != nil {
		result.Progress = &JobProgress{
			Phase:     job.Progress.Phase,
			Message:   job.Progress.Message,
			Completed: job.Progress.Completed,
			Total:     job.Progress.Total,
		}
	}
	if result.Status != JobStatusSucceeded {
		return result, nil
	}
	if len(job.Output) == 0 {
		return JobResult[Out]{}, fmt.Errorf("agentsdk: succeeded job %s has no output", job.ID)
	}
	var output Out
	if err := json.Unmarshal(job.Output, &output); err != nil {
		return JobResult[Out]{}, fmt.Errorf("agentsdk: decode job %s output: %w", job.ID, err)
	}
	result.Output = &output
	return result, nil
}

type jobKey struct {
	name    string
	version int
}

type registeredJob struct {
	name             string
	version          int
	description      string
	timeout          time.Duration
	maxAttempts      int
	maxConcurrency   int
	inputSchema      json.RawMessage
	outputSchema     json.RawMessage
	inputSchemaHash  string
	outputSchemaHash string
	handler          func(context.Context, JobContext, json.RawMessage) (json.RawMessage, error)
}

type registeredJobCron struct {
	slug             string
	schedule         string
	description      string
	handlerName      string
	handlerVersion   int
	inputSchemaHash  string
	outputSchemaHash string
	input            json.RawMessage
}

// handleJob serves one durable background-job delivery attempt and returns its
// terminal result after run bookkeeping reaches Airlock.
func (a *Agent) handleJob(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	versionText := r.PathValue("version")
	version, err := strconv.ParseInt(versionText, 10, 32)
	if err != nil || version <= 0 || strconv.FormatInt(version, 10) != versionText ||
		!localIdentifierPattern.MatchString(name) || r.URL.EscapedPath() != "/job/"+name+"/"+versionText {
		http.Error(w, "invalid job path", http.StatusBadRequest)
		return
	}

	job, ok := a.jobs[jobKey{name: name, version: int(version)}]
	if !ok {
		http.NotFound(w, r)
		return
	}

	var req wire.JobRunRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJobRunRequestBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		http.Error(w, "invalid job run request", http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		http.Error(w, "invalid job run request", http.StatusBadRequest)
		return
	}
	if err := validateJobRunRequest(req, name, int32(version), job); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	leaseToken := r.Header.Get(jobLeaseTokenHeader)
	if leaseToken != "" {
		if err := validateJobID(leaseToken); err != nil {
			http.Error(w, "invalid job lease token header: "+err.Error(), http.StatusBadRequest)
			return
		}
	}

	runID := r.Header.Get("X-Run-ID")
	if runID == "" {
		panic("agentsdk: X-Run-ID header is required")
	}

	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(req.TimeoutMs)*time.Millisecond)
	defer cancel()
	run := newRun(a, runID, r.Header.Get("X-Bridge-ID"), req.InitiatorConversationID, ctx)
	run.userID = req.InitiatorUserID
	run.callerAccess = Access(req.CallerAccess)
	ctx = contextWithRun(ctx, run)
	ctx = contextWithJobRun(ctx, &jobRunContext{agent: a, id: req.ID, attempt: int(req.Attempt), leaseToken: leaseToken})

	status, output, errMsg, panicTrace := executeJob(ctx, job, req)
	runStatus := status
	errorKind := wire.ErrorKindAgent
	if status == "retry" {
		runStatus = "error"
		errorKind = wire.ErrorKindPlatform
	}
	if err := run.complete(ctx, runStatus, errMsg, errorKind, panicTrace); err != nil {
		http.Error(w, "record job run completion: "+err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(wire.JobRunResponse{Status: status, Output: output, Error: errMsg})
}

func validateJobRunRequest(req wire.JobRunRequest, pathName string, pathVersion int32, job *registeredJob) error {
	if err := validateJobID(req.ID); err != nil {
		return fmt.Errorf("invalid job run request: %w", err)
	}
	if req.Attempt <= 0 {
		return errors.New("invalid job run request: Attempt must be positive")
	}
	if req.Name != pathName || req.Version != pathVersion || req.Name != job.name || int(req.Version) != job.version ||
		req.InputSchemaHash != job.inputSchemaHash || req.OutputSchemaHash != job.outputSchemaHash {
		return errors.New("invalid job run request: contract mismatch")
	}
	if req.TimeoutMs <= 0 || req.TimeoutMs != job.timeout.Milliseconds() {
		return errors.New("invalid job run request: timeout mismatch")
	}
	if len(req.Input) == 0 || len(req.Input) > maxJobPayloadBytes {
		return fmt.Errorf("invalid job run request: input must be between 1 and %d bytes", maxJobPayloadBytes)
	}
	if req.ScheduledAt != nil && req.ScheduledAt.IsZero() {
		return errors.New("invalid job run request: ScheduledAt must be nonzero when present")
	}
	var input map[string]json.RawMessage
	if err := json.Unmarshal(req.Input, &input); err != nil || input == nil {
		return errors.New("invalid job run request: input must be a JSON object")
	}
	if err := validateJobInitiator(req); err != nil {
		return fmt.Errorf("invalid job run request: %w", err)
	}
	return nil
}

func validateJobInitiator(req wire.JobRunRequest) error {
	if req.InitiatorConversationID != "" {
		if err := validateJobID(req.InitiatorConversationID); err != nil {
			return errors.New("InitiatorConversationID must be a canonical non-nil UUID")
		}
	}
	switch req.InitiatorKind {
	case "user":
		if err := validateJobID(req.InitiatorUserID); err != nil {
			return errors.New("InitiatorUserID must be a canonical non-nil UUID")
		}
		if req.CallerAccess != wire.AccessUser && req.CallerAccess != wire.AccessAdmin {
			return errors.New("user initiator requires user or admin CallerAccess")
		}
	case "anonymous", "system":
		if req.InitiatorUserID != "" || req.CallerAccess != wire.AccessPublic {
			return fmt.Errorf("%s initiator requires an empty InitiatorUserID and public CallerAccess", req.InitiatorKind)
		}
	default:
		return errors.New("invalid InitiatorKind")
	}
	return nil
}

func executeJob(ctx context.Context, job *registeredJob, req wire.JobRunRequest) (status string, output json.RawMessage, errMsg, panicTrace string) {
	defer func() {
		if recovered := recover(); recovered != nil {
			status = "error"
			output = nil
			errMsg = fmt.Sprintf("%v", recovered)
			panicTrace = string(debug.Stack())
		}
	}()

	output, err := job.handler(ctx, JobContext{ID: req.ID, Attempt: int(req.Attempt), ScheduledAt: req.ScheduledAt}, req.Input)
	if ctx.Err() != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "timeout", nil, ctx.Err().Error(), ""
		}
		return "error", nil, ctx.Err().Error(), ""
	}
	if err != nil {
		if errors.Is(err, ErrJobEnqueueUnavailable) {
			return "retry", nil, err.Error(), ""
		}
		return "error", nil, err.Error(), ""
	}
	if len(output) > maxJobPayloadBytes {
		return "error", nil, fmt.Sprintf("job output exceeds %d bytes", maxJobPayloadBytes), ""
	}
	if len(output) == 0 || !json.Valid(output) {
		return "error", nil, "job output is not valid JSON", ""
	}
	return "success", output, "", ""
}

// RegisterJob registers a typed, versioned background-job handler. The same
// name may have multiple versions so queued work remains executable across
// compatible agent deployments.
func RegisterJob[In, Out any](a *Agent, job *Job[In, Out]) *JobHandle[In, Out] {
	if a == nil {
		panic("agentsdk: RegisterJob: nil *Agent")
	}
	done := a.beginRegistration("RegisterJob")
	defer done()
	if job == nil {
		panic("agentsdk: RegisterJob: nil *Job")
	}
	if job.Handler == nil {
		panic(fmt.Sprintf("agentsdk: RegisterJob(%q): Handler is required", job.Name))
	}

	inputType := reflect.TypeOf((*In)(nil)).Elem()
	if inputType.Kind() != reflect.Struct {
		panic(fmt.Sprintf("agentsdk: RegisterJob(%q): input type must be a struct", job.Name))
	}
	validateJobGoType(job.Name, "input", inputType, make(map[reflect.Type]bool))
	validateJobGoType(job.Name, "output", reflect.TypeOf((*Out)(nil)).Elem(), make(map[reflect.Type]bool))
	input := schema.MustFromType(*new(In))
	if input.Type != "object" {
		panic(fmt.Sprintf("agentsdk: RegisterJob(%q): input type must encode as a JSON object", job.Name))
	}
	inputSchema := canonicalJobSchema(input.MustJSON())
	outputSchema := canonicalJobSchema(schema.MustFromType(*new(Out)).MustJSON())

	rj := &registeredJob{
		name:             job.Name,
		version:          job.Version,
		description:      job.Description,
		timeout:          job.Timeout,
		maxAttempts:      job.MaxAttempts,
		maxConcurrency:   job.MaxConcurrency,
		inputSchema:      inputSchema,
		outputSchema:     outputSchema,
		inputSchemaHash:  hashJobSchema(inputSchema),
		outputSchemaHash: hashJobSchema(outputSchema),
	}
	handler := job.Handler
	rj.handler = func(ctx context.Context, event JobContext, raw json.RawMessage) (json.RawMessage, error) {
		var input In
		if err := json.Unmarshal(raw, &input); err != nil {
			return nil, fmt.Errorf("decode %s@v%d input: %w", rj.name, rj.version, err)
		}
		output, err := handler(ctx, event, input)
		if err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(output)
		if err != nil {
			return nil, fmt.Errorf("encode %s@v%d output: %w", rj.name, rj.version, err)
		}
		return encoded, nil
	}

	validateRegisteredJob(rj)
	key := jobKey{name: rj.name, version: rj.version}
	if _, exists := a.jobs[key]; exists {
		panic(fmt.Sprintf("agentsdk: duplicate RegisterJob: %s@v%d", rj.name, rj.version))
	}
	a.jobs[key] = rj
	return &JobHandle[In, Out]{agent: a, name: rj.name, version: rj.version}
}

func hashJobSchema(value json.RawMessage) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func canonicalJobSchema(value json.RawMessage) json.RawMessage {
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		panic("agentsdk: canonicalize job schema: " + err.Error())
	}
	normalizeJobSchemaSets(decoded)
	canonical, err := json.Marshal(decoded)
	if err != nil {
		panic("agentsdk: canonicalize job schema: " + err.Error())
	}
	return canonical
}

func validateJobGoType(jobName, position string, value reflect.Type, visiting map[reflect.Type]bool) {
	jsonMarshaler := reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	jsonUnmarshaler := reflect.TypeOf((*json.Unmarshaler)(nil)).Elem()
	textMarshaler := reflect.TypeOf((*encoding.TextMarshaler)(nil)).Elem()
	textUnmarshaler := reflect.TypeOf((*encoding.TextUnmarshaler)(nil)).Elem()
	if value.Implements(jsonMarshaler) || value.Implements(jsonUnmarshaler) || value.Implements(textMarshaler) || value.Implements(textUnmarshaler) ||
		(value.Kind() != reflect.Pointer && (reflect.PointerTo(value).Implements(jsonMarshaler) || reflect.PointerTo(value).Implements(jsonUnmarshaler) || reflect.PointerTo(value).Implements(textMarshaler) || reflect.PointerTo(value).Implements(textUnmarshaler))) {
		panic(fmt.Sprintf("agentsdk: RegisterJob(%q): %s type %s has custom JSON encoding unsupported by reflected job schemas", jobName, position, value))
	}
	if visiting[value] {
		panic(fmt.Sprintf("agentsdk: RegisterJob(%q): %s type %s is recursive", jobName, position, value))
	}
	switch value.Kind() {
	case reflect.Pointer:
		visiting[value] = true
		validateJobGoType(jobName, position, value.Elem(), visiting)
		delete(visiting, value)
	case reflect.Struct:
		visiting[value] = true
		names := make(map[string]struct{}, value.NumField())
		for i := 0; i < value.NumField(); i++ {
			field := value.Field(i)
			if field.PkgPath != "" || field.Tag.Get("json") == "-" {
				continue
			}
			if field.Anonymous {
				panic(fmt.Sprintf("agentsdk: RegisterJob(%q): %s field %s is embedded and unsupported by reflected job schemas", jobName, position, field.Name))
			}
			fieldName := field.Name
			if tag := field.Tag.Get("json"); tag != "" {
				parts := strings.Split(tag, ",")
				if parts[0] != "" {
					fieldName = parts[0]
				}
				for _, option := range parts[1:] {
					if option != "" && option != "omitempty" {
						panic(fmt.Sprintf("agentsdk: RegisterJob(%q): %s field %s uses unsupported json option %q", jobName, position, field.Name, option))
					}
				}
			}
			if _, exists := names[fieldName]; exists {
				panic(fmt.Sprintf("agentsdk: RegisterJob(%q): %s has duplicate JSON field %q", jobName, position, fieldName))
			}
			names[fieldName] = struct{}{}
			if field.Tag.Get("enum") != "" && field.Type.Kind() != reflect.String {
				panic(fmt.Sprintf("agentsdk: RegisterJob(%q): %s field %s uses enum on a non-string type", jobName, position, field.Name))
			}
			if field.Tag.Get("default") != "" && field.Type.Kind() != reflect.String {
				panic(fmt.Sprintf("agentsdk: RegisterJob(%q): %s field %s uses default on a non-string type", jobName, position, field.Name))
			}
			validateJobGoType(jobName, position+" field "+field.Name, field.Type, visiting)
		}
		delete(visiting, value)
	case reflect.Slice:
		if value.Elem().Kind() == reflect.Uint8 {
			panic(fmt.Sprintf("agentsdk: RegisterJob(%q): %s type %s encodes as base64 and is unsupported by reflected job schemas", jobName, position, value))
		}
		visiting[value] = true
		validateJobGoType(jobName, position, value.Elem(), visiting)
		delete(visiting, value)
	case reflect.Array:
		visiting[value] = true
		validateJobGoType(jobName, position, value.Elem(), visiting)
		delete(visiting, value)
	case reflect.Map, reflect.Interface:
		panic(fmt.Sprintf("agentsdk: RegisterJob(%q): %s type %s is unsupported by reflected job schemas", jobName, position, value))
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64, reflect.String:
		return
	default:
		panic(fmt.Sprintf("agentsdk: RegisterJob(%q): %s type %s is unsupported", jobName, position, value))
	}
}

func normalizeJobSchemaSets(value any) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if key == "required" {
				if values, ok := child.([]any); ok {
					sort.Slice(values, func(i, j int) bool {
						left, leftOK := values[i].(string)
						right, rightOK := values[j].(string)
						return leftOK && rightOK && left < right
					})
				}
			}
			normalizeJobSchemaSets(child)
		}
	case []any:
		for _, child := range typed {
			normalizeJobSchemaSets(child)
		}
	}
}
