package agenttest_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/airlockrun/agentsdk/agenttest"
	"github.com/airlockrun/agentsdk/jsexec"
	"github.com/airlockrun/agentsdk/wire"
)

func TestExecutorConfigFromEnvRequiresExplicitTransport(t *testing.T) {
	for _, tc := range []struct{ name, endpoint, token string }{
		{"missing", "", ""},
		{"missing token", "http://airlock.test/test", ""},
		{"missing URL", "", "build-token"},
		{"URL credentials", "http://user:pass@airlock.test/test", "build-token"},
		{"URL query", "http://airlock.test/test?token=secret", "build-token"},
		{"invalid scheme", "file:///test", "build-token"},
		{"invalid token", "http://airlock.test/test", "bad\ntoken"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AIRLOCK_TEST_EXECUTOR_URL", tc.endpoint)
			t.Setenv("AIRLOCK_TEST_EXECUTOR_TOKEN", tc.token)
			if _, err := agenttest.ExecutorConfigFromEnv(jsexec.DefaultLimits()); err == nil {
				t.Fatal("accepted incomplete or unsafe transport configuration")
			}
		})
	}
}

func TestExecutorHTTPTransportDuplexAndCancellation(t *testing.T) {
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer build-token" {
			t.Error("missing explicit request authentication")
		}
		c := http.NewResponseController(w)
		if err := c.EnableFullDuplex(); err != nil {
			t.Error(err)
			return
		}
		w.WriteHeader(http.StatusContinue)
		w.Header().Set("Content-Type", wire.TestExecutorContentType)
		if err := c.Flush(); err != nil {
			return
		}
		for {
			var data [4]byte
			if _, err := io.ReadFull(r.Body, data[:]); err != nil {
				return
			}
			if _, err := w.Write(data[:]); err != nil {
				return
			}
			if err := c.Flush(); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	t.Setenv("AIRLOCK_TEST_EXECUTOR_URL", server.URL)
	t.Setenv("AIRLOCK_TEST_EXECUTOR_TOKEN", "build-token")
	config, err := agenttest.ExecutorConfigFromEnv(jsexec.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	stream, err := config.OpenTransport(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	for _, input := range []string{"one!", "two!"} {
		if _, err := stream.Write([]byte(input)); err != nil {
			t.Fatal(err)
		}
		var output [4]byte
		if _, err := io.ReadFull(stream, output[:]); err != nil {
			t.Fatal(err)
		}
		if string(output[:]) != input {
			t.Fatalf("output = %q", output)
		}
	}
	cancel()
	if _, err := stream.Read(make([]byte, 1)); err == nil {
		t.Fatal("cancelled read succeeded")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not close the server request")
	}
}

func TestExecutorHTTPTransportRejectsHandshakeAndRedirect(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusConflict, http.StatusServiceUnavailable, http.StatusTemporaryRedirect, http.StatusOK} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", "http://must-not-receive-token.invalid")
				w.WriteHeader(status)
			}))
			defer server.Close()
			config, err := agenttest.RemoteExecutorConfig(server.URL, "build-token", jsexec.DefaultLimits())
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			if stream, err := config.OpenTransport(ctx); err == nil {
				stream.Close()
				t.Fatal("accepted unsuccessful or wrong-protocol handshake")
			}
		})
	}
}
