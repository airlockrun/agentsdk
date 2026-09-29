package agentsdk

import (
	"context"

	"github.com/airlockrun/goai/tool"
	"github.com/google/uuid"
)

// AsyncTool exposes a typed durable job as an app tool. The job's version,
// timeout, attempts and concurrency are required execution policy. Its eventual
// output uses the job contract; the immediate tool result is a task handle.
type AsyncTool[In, Out any] struct {
	noUnkeyedLiterals
	Job     *Job[In, Out]
	Access  Access
	LLMHint string
}

// AsyncTask identifies accepted asynchronous work. The ID is an opaque handle
// for tasks.get/wait/cancel; it is not a credential or an execution-attempt ID.
type AsyncTask struct {
	ID     string    `json:"id"`
	Status JobStatus `json:"status"`
}

// RegisterAsyncTool registers the job handler and a tool with the same name.
// Invocation enqueues work before returning its ID. The host owns delivery,
// cancellation and recovery; no handler goroutine is detached in the app.
// The returned native handle can also declare crons or inspect typed job output.
func RegisterAsyncTool[In, Out any](a *Agent, definition *AsyncTool[In, Out]) *JobHandle[In, Out] {
	if a == nil || definition == nil || definition.Job == nil {
		panic("agentsdk: async tool requires an agent and job definition")
	}
	validateToolAccess("RegisterAsyncTool", definition.Access)
	handle := RegisterJob(a, definition.Job)
	registered := tool.Typed[In, AsyncTask](definition.Job.Name).
		Description(definition.Job.Description + " Returns a durable task handle immediately. Use tasks.get, tasks.wait or tasks.cancel with its id; the task's output is available on successful completion.").
		Execute(func(ctx context.Context, input In) (AsyncTask, error) {
			result, err := handle.Enqueue(ctx, uuid.NewString(), input)
			if err != nil {
				return AsyncTask{}, err
			}
			return AsyncTask{ID: "job:" + result.ID, Status: result.Status}, nil
		}).Build()
	a.RegisterTool(registered, definition.Access, WithLLMHint(definition.LLMHint))
	return handle
}
