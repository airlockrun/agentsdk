package chatruntime

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/airlockrun/agentsdk/jsexec"
)

func TestExecutorFromTransportIsLazyAndRequiresExplicitDependencies(t *testing.T) {
	if _, err := ExecutorFromTransport(nil, jsexec.DefaultLimits(), nil); err == nil {
		t.Fatal("accepted missing opener")
	}
	opens := 0
	problem := errors.New("executor unavailable")
	open := func(context.Context) (io.ReadWriteCloser, error) { opens++; return nil, problem }
	if _, err := ExecutorFromTransport(open, jsexec.Limits{}, nil); err == nil {
		t.Fatal("accepted absent limits")
	}
	factory, err := ExecutorFromTransport(open, jsexec.DefaultLimits(), nil)
	if err != nil || opens != 0 {
		t.Fatalf("factory = %v, opens = %d", err, opens)
	}
	if _, err := factory(t.Context(), nil); !errors.Is(err, problem) || opens != 1 {
		t.Fatalf("session = %v, opens = %d", err, opens)
	}
	nilFactory, err := ExecutorFromTransport(func(context.Context) (io.ReadWriteCloser, error) { return nil, nil }, jsexec.DefaultLimits(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := nilFactory(t.Context(), nil); err == nil {
		t.Fatal("accepted nil transport")
	}
}
