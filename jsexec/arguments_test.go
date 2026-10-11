package jsexec

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
)

func TestPublishedStructuredArguments(t *testing.T) {
	if os.Getenv("JSEXEC_DOCKER_TEST") != "1" {
		t.Skip("set JSEXEC_DOCKER_TEST=1 for published executor argument validation")
	}
	image, err := PrepareDockerImage(t.Context(), PublishedExecutorImage)
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewDockerSession(t.Context(), image, Options{Limits: DefaultLimits()})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	args := json.RawMessage(`{"text":"\";throw Error('injection');//\nΩ","nested":{"value":7}}`)
	callback := InvokerFunc(func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
		return nil, errors.New("unexpected host callback")
	})
	out, err := s.ExecuteWithArgs(t.Context(), `return args;`, args, callback)
	if err != nil || string(out.Output) != string(args) {
		t.Fatalf("structured args lost or evaluated: %s %v", out.Output, err)
	}
	if _, err := s.ExecuteWithArgs(t.Context(), `console.warn('partial'); throw new TypeError('saved failure');`, json.RawMessage(`{}`), callback); err == nil {
		t.Fatal("saved script exception swallowed")
	}
	for _, bad := range []json.RawMessage{nil, []byte(`null`), []byte(`[]`), []byte(`"code"`)} {
		if _, err := s.ExecuteWithArgs(t.Context(), `return 1;`, bad, callback); err == nil {
			t.Fatal("invalid args executed")
		}
	}
}
