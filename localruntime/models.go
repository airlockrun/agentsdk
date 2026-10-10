package localruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/airlockrun/agentsdk/internal/streamevent"
	"github.com/airlockrun/goai/stream"
)

// ModelHandler implements the SDK streaming model proxy using explicit bindings.
func ModelHandler(models map[string]stream.Model) http.Handler {
	bindings := make(map[string]stream.Model, len(models))
	for slot, model := range models {
		if model == nil {
			panic("mockairlock: nil streaming model")
		}
		bindings[slot] = model
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { streamModel(bindings, w, r) })
}

func streamModel(models map[string]stream.Model, w http.ResponseWriter, r *http.Request) {
	// The proxy wraps its serialized stream.CallOptions with routing coordinates.
	var request struct {
		ModelID    string              `json:"model_id"`
		Slug       string              `json:"slug"`
		Capability string              `json:"capability"`
		Options    *stream.CallOptions `json:"options"`
	}
	if err := strictJSON(r.Body, &request); err != nil || request.Options == nil {
		http.Error(w, "mockairlock: invalid streaming model request", http.StatusBadRequest)
		return
	}
	if request.Capability != "text" && request.Capability != "vision" {
		http.Error(w, "mockairlock: streaming capability must be text or vision", http.StatusBadRequest)
		return
	}
	if request.Slug == "" && request.Capability != "text" {
		http.Error(w, "mockairlock: default streaming binding requires text capability", http.StatusBadRequest)
		return
	}
	model, ok := models[request.Slug]
	if !ok {
		w.Header().Set("X-Airlock-LLM-Retryable", "false")
		http.Error(w, fmt.Sprintf("mockairlock: streaming model slot %q is not bound", request.Slug), http.StatusBadRequest)
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
