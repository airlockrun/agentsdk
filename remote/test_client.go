package remote

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/structpb"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// TestConfig is supplied by the host runner, not a laptop login or app token.
// The API root is an exact /api/test-runs/UUID capability namespace.
type TestConfig struct {
	APIRoot, Token, AppID, DeploymentID, Revision string
	RuntimeGeneration                             int64
	HTTPClient                                    *http.Client
}
type TestClient struct{ api *Client }

func NewTestClient(config TestConfig) (*TestClient, error) {
	u, err := url.Parse(config.APIRoot)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" {
		return nil, errors.New("test client requires an explicit scoped HTTP(S) root")
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(parts) != 3 || parts[0] != "api" || parts[1] != "test-runs" || !canonicalID(parts[2]) || !canonicalID(config.AppID) || !canonicalID(config.DeploymentID) || !canonicalID(config.Revision) || config.RuntimeGeneration < 1 || config.Token == "" || strings.ContainsAny(config.Token, " \t\r\n") || config.HTTPClient == nil {
		return nil, errors.New("test client requires exact runner root, app/deployment/revision/generation, capability and HTTP client")
	}
	origin := u.Scheme + "://" + u.Host
	api := &Client{config: Config{URL: origin, AppID: config.AppID, Revision: config.Revision, HTTPClient: config.HTTPClient, Token: func(context.Context) (string, error) { return config.Token, nil }}, scopedPath: u.Path}
	return &TestClient{api: api}, nil
}
func canonicalID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id != uuid.Nil && id.String() == value
}

// TestClientFromEnvironment reads only explicit opaque runner coordinates.
// No CLI login store, provider account, database DSN or host emulator is consulted.
func TestClientFromEnvironment() (*TestClient, error) {
	generation, err := strconv.ParseInt(os.Getenv("AIRLOCK_TEST_RUNTIME_GENERATION"), 10, 64)
	if err != nil {
		return nil, errors.New("AIRLOCK_TEST_RUNTIME_GENERATION is required")
	}
	return NewTestClient(TestConfig{APIRoot: os.Getenv("AIRLOCK_TEST_API_URL"), Token: os.Getenv("AIRLOCK_TEST_TOKEN"), AppID: os.Getenv("AIRLOCK_TEST_APP_ID"), DeploymentID: os.Getenv("AIRLOCK_TEST_DEPLOYMENT_ID"), Revision: os.Getenv("AIRLOCK_TEST_SOURCE_REVISION"), RuntimeGeneration: generation, HTTPClient: &http.Client{Timeout: 2 * time.Minute}})
}

// The scoped client exposes execution only, never binding/deployment/inventory.
func (c *TestClient) Request(ctx context.Context, method, path string, body io.Reader, headers http.Header) (*http.Response, error) {
	return c.api.Request(ctx, method, path, body, headers)
}
func (c *TestClient) InvokeTool(ctx context.Context, name string, input *structpb.Value) (*ToolResult, error) {
	return c.api.InvokeTool(ctx, name, input)
}
func (c *TestClient) StartAgent(ctx context.Context, definition, requestID, contractHash string, input *structpb.Value) (*AgentRun, error) {
	return c.api.StartAgent(ctx, definition, requestID, contractHash, input)
}
func (c *TestClient) GetAgent(ctx context.Context, definition, id string) (*AgentRun, error) {
	return c.api.GetAgent(ctx, definition, id)
}
func (c *TestClient) WaitAgent(ctx context.Context, definition, id string, poll time.Duration) (*AgentRun, error) {
	return c.api.WaitAgent(ctx, definition, id, poll)
}
func (c *TestClient) CancelAgent(ctx context.Context, definition, id string) (*AgentRun, error) {
	return c.api.CancelAgent(ctx, definition, id)
}
