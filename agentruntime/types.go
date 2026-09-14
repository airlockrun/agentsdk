package agentruntime

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/airlockrun/agentsdk/capability"
	"github.com/airlockrun/agentsdk/chatruntime"
	"github.com/airlockrun/agentsdk/wire"
	"github.com/airlockrun/goai/stream"
	"github.com/airlockrun/sol/eventstream"
	"github.com/airlockrun/sol/session"
)

// ErrWaiting parks a run without completing it. Run returns both a Waiting
// result and this error. The host releases its worker and resumes on wakeup.
var ErrWaiting = errors.New("agentruntime: waiting for agent calls")

type Input struct {
	Definition wire.AgentDefinition
	// Subagents supplies the exact contracts named by Definition.Subagents.
	Subagents       []wire.AgentDefinition
	Message         string
	Model           stream.Model
	ModelLimits     session.ModelLimits
	Store           Store
	Capabilities    []capability.Definition
	Backend         chatruntime.Backend
	ExecutorFactory chatruntime.ExecutorFactory
	Controller      Controller
	Sink            eventstream.Sink
	Redactor        func(string) string
	RecoveryNotice  string
}

type Store interface {
	session.SessionStore
	LoadCheckpoint(context.Context) (*Checkpoint, error)
	// SaveCheckpoint atomically appends Append and saves the checkpoint. Revision
	// starts at 1 and increases by one. Reject stale writers; an identical retry
	// of a committed revision must not append messages twice. Append is a commit
	// payload, not a buffer to replay on LoadCheckpoint. See package documentation.
	SaveCheckpoint(context.Context, *Checkpoint) error
}

type Controller interface {
	BeforeModel(ctx context.Context) error
	Spawn(ctx context.Context, toolCallID, definitionSlug string, input json.RawMessage) (wire.AgentRunInfo, error)
	Continue(ctx context.Context, toolCallID, sessionID, prompt string) (wire.AgentRunInfo, error)
	Get(ctx context.Context, ids []string) ([]wire.AgentRunInfo, error)
	Wait(ctx context.Context, toolCallID string, request wire.AgentWaitRequest) (wire.AgentWaitResult, error)
	Cancel(ctx context.Context, ids []string) (wire.AgentCancelResult, error)
	Complete(ctx context.Context, reply wire.AgentReply) error
}

type Result struct {
	Reply   *wire.AgentReply
	Waiting bool
	// CleanupError reports executor teardown failure separately from a durably
	// committed reply. The host must log it without changing completed status.
	// For waiting results it is also joined into Run's returned error. Unfinished
	// runs without a Result report cleanup failure only in the returned error.
	// This attempt-local diagnostic is not part of the durable checkpoint.
	CleanupError error
}

// Checkpoint is a JSON-serializable journal scoped to one run, not its session.
// The host must preserve it verbatim and must not infer completion from Phase
// until the associated transaction commits. Version is currently 1.
type Checkpoint struct {
	Version      int               `json:"version"`
	Revision     int64             `json:"revision"`
	ContractHash string            `json:"contractHash"`
	Phase        Phase             `json:"phase"`
	Response     []session.Message `json:"response,omitempty"`
	Calls        []Call            `json:"calls,omitempty"`
	CallIDs      []string          `json:"callIds,omitempty"`
	Next         int               `json:"next"`
	Reply        *wire.AgentReply  `json:"reply,omitempty"`
	Append       []session.Message `json:"append,omitempty"`
}

type Phase string

const (
	PhaseReady     Phase = "ready"
	PhaseModel     Phase = "model"
	PhaseTools     Phase = "tools"
	PhaseDispatch  Phase = "dispatch"
	PhaseWaiting   Phase = "waiting"
	PhaseCompleted Phase = "completed"
)

// Call records model order. Result is set only after the outcome is committed.
type Call struct {
	ID     string           `json:"id"`
	Name   string           `json:"name"`
	Input  json.RawMessage  `json:"input"`
	Result *session.Message `json:"result,omitempty"`
}
