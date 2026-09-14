package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/airlockrun/agentsdk/chatruntime"
	"github.com/airlockrun/goai"
	"github.com/airlockrun/goai/message"
	"github.com/airlockrun/goai/stream"
	"github.com/airlockrun/goai/tool"
	"github.com/airlockrun/sol/session"
	"github.com/google/uuid"
)

// Run advances a durable task until typed completion, parking, or failure. A
// resumed batch never asks the model to regenerate already captured calls.
func Run(ctx context.Context, in Input) (result *Result, err error) {
	if in.Model == nil || in.Store == nil || in.Backend == nil || in.ExecutorFactory == nil || in.Controller == nil || in.Sink == nil || in.Redactor == nil {
		return nil, errors.New("agentruntime: Model, Store, Backend, ExecutorFactory, Controller, Sink and Redactor are required")
	}
	if err := in.ModelLimits.Validate(true); err != nil {
		return nil, err
	}
	budget := in.Definition.Budget
	if budget.Steps < 0 || budget.TimeoutMS < 0 || budget.Tokens < 0 || budget.Steps == 0 && budget.TimeoutMS == 0 && budget.Tokens == 0 {
		return nil, errors.New("agentruntime: an explicit nonnegative budget with at least one positive limit is required")
	}
	if in.Definition.Slug == "" || in.Definition.ContractHash == "" || strings.TrimSpace(in.Definition.Instructions) == "" {
		return nil, errors.New("agentruntime: definition identity and instructions are required")
	}
	js, closeJS, err := chatruntime.JavaScriptTool(ctx, in.Capabilities, in.Backend, in.ExecutorFactory)
	if err != nil {
		return nil, err
	}
	defer func() {
		cleanupErr := closeJS()
		if cleanupErr == nil {
			return
		}
		if result != nil {
			result.CleanupError = cleanupErr
			if result.Reply != nil && err == nil {
				return
			}
		}
		err = errors.Join(err, cleanupErr)
	}()
	set, spawns, err := tools(in, js)
	if err != nil {
		return nil, err
	}
	declarations, err := chatruntime.RenderTypeScript(in.Capabilities)
	if err != nil {
		return nil, err
	}
	prompt := in.Redactor(in.Definition.Instructions + "\n\n" + instructions + "\n\nJavaScript capabilities:\n```typescript\n" + declarations + "```")
	cp, err := in.Store.LoadCheckpoint(ctx)
	if err != nil {
		return nil, fmt.Errorf("load checkpoint: %w", err)
	}
	save := func(phase Phase, appendMessages ...session.Message) error {
		cp.Phase, cp.Append = phase, appendMessages
		cp.Revision++
		for i := range cp.Append {
			if cp.Append[i].ID == "" {
				cp.Append[i].ID = uuid.NewString()
			}
		}
		if err := in.Store.SaveCheckpoint(ctx, cp); err != nil {
			return fmt.Errorf("save checkpoint: %w", err)
		}
		cp.Append = nil
		return nil
	}
	if cp == nil {
		if strings.TrimSpace(in.Message) == "" {
			return nil, errors.New("agentruntime: a fresh run requires Message")
		}
		cp = &Checkpoint{Version: 1, ContractHash: in.Definition.ContractHash}
		if err := save(PhaseReady, session.Message{Role: "user", Content: in.Redactor(in.Message)}); err != nil {
			return nil, err
		}
	} else {
		if err := validateCheckpoint(cp, in.Definition.ContractHash); err != nil {
			return nil, err
		}
		cp.Append = nil
		if cp.Phase == PhaseCompleted {
			return &Result{Reply: cp.Reply}, nil
		}
		if in.RecoveryNotice != "" {
			// Keep tool-call/result messages contiguous. Host notices are delivered
			// with the next model request, after any saved batch is resolved.
			prompt += "\n\nHost recovery notice: " + in.Redactor(in.RecoveryNotice)
		}
		if cp.Phase == PhaseModel {
			if err := save(PhaseReady, session.Message{Role: "user", Content: "Recovery notice: the previous model request was interrupted before a complete response was checkpointed. No local tools from that response were dispatched. JavaScript state is empty."}); err != nil {
				return nil, err
			}
		}
		if cp.Phase == PhaseDispatch {
			call := cp.Calls[cp.Next]
			_, spawn := spawns[call.Name]
			if !spawn && call.Name != "continue_agent" && call.Name != "wait_calls" && call.Name != "complete" {
				out := message.ErrorTextOutput{Value: "Recovery notice: interrupted tool " + call.Name + " (" + call.ID + ") has an unknown outcome. Its side effects may have occurred. It was not retried. JavaScript state is empty; do not automatically repeat this effect."}
				msg := session.FromGoAIMessage(message.NewToolMessage(call.ID, call.Name, out))
				msg.ID = uuid.NewString()
				cp.Calls[cp.Next].Result = &msg
				cp.Next++
				if err := save(PhaseTools, msg); err != nil {
					return nil, err
				}
				in.Sink.OnToolResult(stream.ToolResultEvent{ToolCallID: call.ID, ToolName: call.Name, Output: out})
			}
		}
	}
	// Definitions have no Execute function in the model call. goai captures the
	// complete response without dispatch; only this checkpointed loop executes.
	capture := tool.Set{}
	for name, t := range set {
		t.Execute = nil
		capture[name] = t
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if cp.Phase == PhaseReady {
			history, err := in.Store.Load(ctx)
			if err != nil {
				return nil, fmt.Errorf("load transcript: %w", err)
			}
			history, err = compact(ctx, in, history, prompt, capture)
			if err != nil {
				return nil, err
			}
			cp.Response, cp.Calls, cp.Next = nil, nil, 0
			if err := save(PhaseModel); err != nil {
				return nil, err
			}
			if err := in.Controller.BeforeModel(ctx); err != nil {
				return nil, err
			}
			var response []session.Message
			generated, err := goai.StreamText(ctx, stream.Input{
				Model: in.Model, Messages: session.MessagesToGoAI(redactMessages(history, in.Redactor)), Instructions: prompt,
				Tools: capture, ToolChoice: "required", MaxSteps: 1, MaxRetries: 0, MaxRetriesSet: true,
				OnStepEnd: func(step stream.StepResultData) {
					response = session.FromGoAIMessages(step.(*goai.StepResult).Response.Messages)
				},
			})
			if err != nil {
				return nil, err
			}
			for range generated.FullStream {
			}
			finish, err := generated.FinishReason()
			if err != nil {
				return nil, err
			}
			if finish != stream.FinishReasonToolCalls && finish != stream.FinishReasonStop {
				return nil, fmt.Errorf("agentruntime: incomplete model response: %s", finish)
			}
			seen := make(map[string]bool, len(cp.CallIDs))
			for _, id := range cp.CallIDs {
				seen[id] = true
			}
			for _, call := range generated.ToolCalls() {
				if call.ProviderExecuted {
					return nil, errors.New("agentruntime: provider-executed tools are not supported")
				}
				if call.ID == "" || seen[call.ID] || !json.Valid(call.Input) {
					return nil, errors.New("agentruntime: model returned an invalid or reused tool call ID/input")
				}
				seen[call.ID] = true
				cp.CallIDs = append(cp.CallIDs, call.ID)
				cp.Calls = append(cp.Calls, Call{ID: call.ID, Name: call.Name, Input: call.Input})
			}
			cp.Response = redactMessages(response, in.Redactor)
			usage, err := generated.Usage()
			if err != nil {
				return nil, err
			}
			if len(cp.Response) != 0 {
				cp.Response[0].Tokens = session.Tokens{Input: usage.InputTotal(), Output: usage.OutputTotal()}
			}
			if err := save(PhaseTools, cp.Response...); err != nil {
				return nil, err
			}
			text, err := generated.Text()
			if err != nil {
				return nil, err
			}
			if text != "" {
				in.Sink.OnTextDelta(stream.TextDeltaEvent{Text: in.Redactor(text)})
			}
		}
		for cp.Next < len(cp.Calls) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			call := cp.Calls[cp.Next]
			if err := save(PhaseDispatch); err != nil {
				return nil, err
			}
			in.Sink.OnToolCall(stream.ToolCallEvent{ToolCallID: call.ID, ToolName: call.Name, Input: redactJSON(call.Input, in.Redactor)})
			out, reply, callErr := dispatch(ctx, in, set, spawns, call)
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if errors.Is(callErr, context.Canceled) || errors.Is(callErr, context.DeadlineExceeded) {
				return nil, callErr
			}
			if errors.Is(callErr, ErrWaiting) {
				if call.Name != "wait_calls" {
					return nil, errors.New("agentruntime: only wait_calls may park")
				}
				if err := save(PhaseWaiting); err != nil {
					return nil, err
				}
				return &Result{Waiting: true}, ErrWaiting
			}
			var fatal tool.FatalToolError
			if errors.As(callErr, &fatal) && fatal.FatalToolError() {
				return nil, callErr
			}
			var output message.ToolResultOutput = message.TextOutput{Value: in.Redactor(out.Output)}
			if callErr != nil {
				output = message.ErrorTextOutput{Value: in.Redactor(callErr.Error())}
				if call.Name == "run_js" {
					output = message.ErrorTextOutput{Value: in.Redactor(callErr.Error()) + "\nThe script did not complete successfully. Earlier capability side effects may have occurred; do not automatically repeat the script."}
				}
			}
			if reason, denied := tool.DenialReason(callErr); denied {
				output = message.ExecutionDeniedOutput{Reason: in.Redactor(reason)}
			}
			msg := message.NewToolMessage(call.ID, call.Name, output)
			if callErr == nil {
				for _, a := range out.Attachments {
					msg.Content.Parts = append(msg.Content.Parts, message.FilePart{Data: message.FileDataBytes{Data: a.Data}, MimeType: a.MimeType, Filename: a.Filename})
				}
			}
			persisted := session.FromGoAIMessage(msg)
			persisted.ID = uuid.NewString()
			cp.Calls[cp.Next].Result = &persisted
			cp.Next++
			pending := []session.Message{persisted}
			phase := PhaseTools
			if reply != nil {
				cp.Reply, phase = reply, PhaseCompleted
				for cp.Next < len(cp.Calls) {
					skipped := cp.Calls[cp.Next]
					m := session.FromGoAIMessage(message.NewToolMessage(skipped.ID, skipped.Name, message.ExecutionDeniedOutput{Reason: "Not executed: the run completed earlier in this batch."}))
					m.ID = uuid.NewString()
					cp.Calls[cp.Next].Result = &m
					pending = append(pending, m)
					cp.Next++
				}
			}
			if err := save(phase, pending...); err != nil {
				return nil, err
			}
			in.Sink.OnToolResult(stream.ToolResultEvent{ToolCallID: call.ID, ToolName: call.Name, Input: redactJSON(call.Input, in.Redactor), Output: output})
			if reply != nil {
				return &Result{Reply: reply}, nil
			}
		}
		var reminder []session.Message
		if len(cp.Calls) == 0 {
			reminder = []session.Message{{Role: "user", Content: "The run is not complete. Call complete with a typed output or a needs_input question."}}
		}
		if err := save(PhaseReady, reminder...); err != nil {
			return nil, err
		}
	}
}

func validateCheckpoint(cp *Checkpoint, hash string) error {
	if cp.Version != 1 || cp.Revision < 1 || cp.ContractHash != hash || cp.Next < 0 || cp.Next > len(cp.Calls) {
		return errors.New("agentruntime: invalid or incompatible checkpoint")
	}
	switch cp.Phase {
	case PhaseReady, PhaseModel, PhaseTools:
		if cp.Reply != nil {
			return errors.New("agentruntime: reply in incomplete checkpoint")
		}
		if cp.Phase != PhaseTools && cp.Next != len(cp.Calls) {
			return errors.New("agentruntime: pending calls at model boundary")
		}
	case PhaseDispatch, PhaseWaiting:
		if cp.Next == len(cp.Calls) || cp.Reply != nil {
			return errors.New("agentruntime: checkpoint has no pending call")
		}
		if cp.Phase == PhaseWaiting && cp.Calls[cp.Next].Name != "wait_calls" {
			return errors.New("agentruntime: waiting checkpoint is not a wait call")
		}
	case PhaseCompleted:
		if cp.Reply == nil || cp.Next != len(cp.Calls) {
			return errors.New("agentruntime: incomplete completion checkpoint")
		}
	default:
		return fmt.Errorf("agentruntime: unknown checkpoint phase %q", cp.Phase)
	}
	seen := map[string]bool{}
	for _, id := range cp.CallIDs {
		if id == "" || seen[id] {
			return errors.New("agentruntime: invalid checkpoint call IDs")
		}
		seen[id] = true
	}
	batch := map[string]bool{}
	for i, call := range cp.Calls {
		if !seen[call.ID] || batch[call.ID] || !json.Valid(call.Input) || (i < cp.Next) != (call.Result != nil) {
			return errors.New("agentruntime: invalid checkpoint call journal")
		}
		batch[call.ID] = true
	}
	return nil
}
