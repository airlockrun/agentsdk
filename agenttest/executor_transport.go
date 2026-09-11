package agenttest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/airlockrun/agentsdk/jsexec"
	"github.com/airlockrun/agentsdk/wire"
)

// ExecutorConfigFromEnv explicitly selects the Airlock-provisioned test
// transport. go tool air build and go test inherit these build-scoped values.
// Missing configuration is an error, never a Docker or in-process fallback.
func ExecutorConfigFromEnv(limits jsexec.Limits) (ExecutorConfig, error) {
	return RemoteExecutorConfig(os.Getenv("AIRLOCK_TEST_EXECUTOR_URL"), os.Getenv("AIRLOCK_TEST_EXECUTOR_TOKEN"), limits)
}

// RemoteExecutorConfig uses a scoped test endpoint. Airlock owns the disposable
// executor; the test process receives only its framed stdin/stdout transport.
func RemoteExecutorConfig(endpoint, token string, limits jsexec.Limits) (ExecutorConfig, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return ExecutorConfig{}, errors.New("agenttest: explicit HTTP(S) test executor URL is required")
	}
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return ExecutorConfig{}, errors.New("agenttest: explicit test executor token is required")
	}
	return ExecutorConfig{Limits: limits, OpenTransport: func(ctx context.Context) (io.ReadWriteCloser, error) {
		ctx, cancel := context.WithCancel(ctx)
		reader, writer := io.Pipe()
		context.AfterFunc(ctx, func() {
			_ = reader.CloseWithError(ctx.Err())
			_ = writer.CloseWithError(ctx.Err())
		})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, reader)
		if err != nil {
			cancel()
			_ = reader.Close()
			_ = writer.Close()
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", wire.TestExecutorContentType)
		req.Header.Set("Expect", "100-continue")
		client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := client.Do(req)
		if err != nil {
			cancel()
			_ = reader.Close()
			_ = writer.Close()
			return nil, err
		}
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != wire.TestExecutorContentType {
			cancel()
			_ = reader.Close()
			_ = writer.Close()
			_ = resp.Body.Close()
			return nil, fmt.Errorf("agenttest: test executor handshake failed (HTTP %d)", resp.StatusCode)
		}
		return &executorHTTPTransport{reader: resp.Body, writer: writer, cancel: cancel}, nil
	}}, nil
}

type executorHTTPTransport struct {
	reader io.ReadCloser
	writer *io.PipeWriter
	cancel context.CancelFunc
	once   sync.Once
	readMu sync.Mutex
	err    error
}

func (s *executorHTTPTransport) Read(p []byte) (int, error) {
	s.readMu.Lock()
	defer s.readMu.Unlock()
	return s.reader.Read(p)
}
func (s *executorHTTPTransport) Write(p []byte) (int, error) { return s.writer.Write(p) }
func (s *executorHTTPTransport) Close() error {
	s.once.Do(func() {
		// EOF asks Airlock to destroy the realm. Its response EOF acknowledges
		// cleanup, so the next sequential test can acquire this build's slot.
		err := s.writer.Close()
		timer := time.AfterFunc(10*time.Second, s.cancel)
		defer timer.Stop()
		defer s.cancel()
		s.readMu.Lock()
		_, drainErr := io.Copy(io.Discard, s.reader)
		s.err = errors.Join(err, drainErr, s.reader.Close())
		s.readMu.Unlock()
	})
	return s.err
}
