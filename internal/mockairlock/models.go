package mockairlock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/airlockrun/agentsdk/internal/streamevent"
	"github.com/airlockrun/goai/stream"
	"net/http"
)

func NewWithModels(models map[string]stream.Model) (*Mock, string) {
	if len(models) == 0 {
		panic("mockairlock: explicit streaming models are required")
	}
	for _, model := range models {
		if model == nil {
			panic("mockairlock: nil streaming model")
		}
	}
	return newMock(models)
}

func (m *Mock) streamModel(w http.ResponseWriter, r *http.Request) {
	var request struct {
		ModelID    string              `json:"model_id"`
		Slug       string              `json:"slug"`
		Capability string              `json:"capability"`
		Options    *stream.CallOptions `json:"options"`
	}
	if err := strictJSON(r.Body, &request); err != nil || request.Options == nil {
		http.Error(w, "mockairlock: invalid streaming model request", 400)
		return
	}
	if request.Capability != "text" && request.Capability != "vision" {
		http.Error(w, "mockairlock: streaming capability must be text or vision", 400)
		return
	}
	if request.Slug == "" && request.Capability != "text" {
		http.Error(w, "mockairlock: default streaming binding requires text capability", 400)
		return
	}
	model, ok := m.models[request.Slug]
	if !ok {
		w.Header().Set("X-Airlock-LLM-Retryable", "false")
		http.Error(w, fmt.Sprintf("mockairlock: streaming model slot %q is not bound", request.Slug), 400)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	request.Options.AbortSignal = ctx
	events, err := model.Stream(ctx, request.Options)
	if err == nil && events == nil {
		err = errors.New("mockairlock: model returned a nil event stream")
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	encoder := json.NewEncoder(w)
	write := func(event stream.Event) error {
		if err := encoder.Encode(streamevent.FromEvent(event)); err != nil {
			return err
		}
		return http.NewResponseController(w).Flush()
	}
	if err != nil {
		_ = write(stream.Event{Type: stream.EventError, Data: stream.ErrorEvent{Error: err}})
		return
	}
	if err := http.NewResponseController(w).Flush(); err != nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-events:
			if !ok {
				return
			}
			if err := write(event); err != nil {
				return
			}
		}
	}
}
