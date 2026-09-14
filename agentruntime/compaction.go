package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/airlockrun/goai"
	"github.com/airlockrun/goai/tool"
	"github.com/airlockrun/sol/bus"
	"github.com/airlockrun/sol/session"
	"github.com/google/uuid"
)

// Compaction runs only at a model boundary, never between a pending tool call
// and its result. Store.Compact is independently atomic, so restart can always
// load a complete context window without changing the call journal.
func compact(ctx context.Context, in Input, history []session.Message, prompt string, tools tool.Set) (_ []session.Message, err error) {
	s := session.New("", in.Definition.Slug, in.Model.ID(), in.ModelLimits)
	defer s.Cancel()
	s.Messages = redactMessages(history, in.Redactor)
	encodedTools, err := json.Marshal(tools.Ordered(nil))
	if err != nil {
		return nil, err
	}
	overhead := session.EstimateTokens(prompt) + session.EstimateTokens(string(encodedTools))
	estimate := session.EstimateMessagesTokens(s.Messages) + overhead
	// Persisted provider usage includes prompt/tool overhead. Account for results
	// appended after that response without counting earlier messages twice.
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].Tokens.Input > 0 {
			estimate = max(estimate, history[i].Tokens.Input+history[i].Tokens.Output+session.EstimateMessagesTokens(history[i+1:]))
			break
		}
	}
	s.Tokens.Input = estimate
	if !s.IsOverflow() {
		return s.Messages, nil
	}
	in.Sink.OnAutomaticCompactionStarted(bus.AutomaticCompactionStartedPayload{})
	freed := 0
	defer func() {
		event := bus.AutomaticCompactionFinishedPayload{TokensFreed: freed}
		if err != nil {
			event.Error = in.Redactor(err.Error())
		}
		in.Sink.OnAutomaticCompactionFinished(event)
	}()
	s.Prune()
	if s.IsOverflow() {
		if err := in.Controller.BeforeModel(ctx); err != nil {
			return nil, err
		}
		if err := s.CompactAndContinue(ctx, in.Model, s.ToGoAIMessages(), &session.CompactOptions{
			MaxOutputTokens: in.ModelLimits.Output,
			TransformMessages: func(msgs []goai.Message) []goai.Message {
				return session.MessagesToGoAI(redactMessages(session.FromGoAIMessages(msgs), in.Redactor))
			},
		}); err != nil {
			return nil, fmt.Errorf("compact context: %w", err)
		}
	}
	s.Messages = redactMessages(s.Messages, in.Redactor)
	for i := range s.Messages {
		s.Messages[i].Tokens = session.Tokens{}
		if s.Messages[i].ID == "" {
			s.Messages[i].ID = uuid.NewString()
		}
	}
	freed = max(0, estimate-session.EstimateMessagesTokens(s.Messages)-overhead)
	if err := in.Store.Compact(ctx, s.Messages, freed); err != nil {
		return nil, fmt.Errorf("persist compaction: %w", err)
	}
	s.Tokens = session.Tokens{Input: session.EstimateMessagesTokens(s.Messages) + overhead}
	if s.IsOverflow() {
		return nil, errors.New("agentruntime: compacted context and tool definitions exceed model input limit")
	}
	return s.Messages, nil
}
