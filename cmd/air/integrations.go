package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/airlockrun/goai/mcp"
)

type integrationTarget struct {
	baseURL string
	agentID string
	token   string
	codegen bool
}

type integrationTargetFlags struct {
	url    string
	remote string
	agent  string
	target string
}

func resolveIntegrationTarget(ctx context.Context, flags integrationTargetFlags) (integrationTarget, error) {
	baseURL := os.Getenv("AIRLOCK_API_URL")
	agentID := os.Getenv("AIRLOCK_AGENT_ID")
	if os.Getenv("AIRLOCK_DEVELOPMENT_SESSION") != "" {
		selected, err := resolveDeployment(ctx, targetFlags{dir: ".", url: flags.url, remote: flags.remote, agent: flags.agent, target: flags.target}, false)
		if err != nil {
			return integrationTarget{}, err
		}
		return integrationTarget{baseURL: selected.baseURL, agentID: selected.info.Id, token: selected.token}, nil
	}
	token := os.Getenv("AIRLOCK_INTEGRATION_TOKEN")
	if baseURL != "" || agentID != "" || token != "" {
		if baseURL == "" || agentID == "" || token == "" {
			return integrationTarget{}, errors.New("AIRLOCK_API_URL, AIRLOCK_AGENT_ID, and AIRLOCK_INTEGRATION_TOKEN must be set together")
		}
		if flags != (integrationTargetFlags{}) {
			return integrationTarget{}, errors.New("--url, --remote, and --agent are unavailable with a codegen integration token")
		}
		return integrationTarget{baseURL: normalizeBaseURL(baseURL), agentID: agentID, token: token, codegen: true}, nil
	}

	selected, err := resolveDeployment(ctx, targetFlags{dir: ".", url: flags.url, remote: flags.remote, agent: flags.agent, target: flags.target}, false)
	if err != nil {
		return integrationTarget{}, err
	}
	return integrationTarget{baseURL: selected.baseURL, agentID: selected.info.Id, token: selected.token}, nil
}

func (t integrationTarget) path(suffix string) string {
	if !t.codegen {
		panic("air: build-bound integration path requires codegen authority")
	}
	return "/api/codegen/integrations" + suffix
}

func consumeIntegrationTargetFlag(args []string, index *int, flags *integrationTargetFlags) (bool, error) {
	key := args[*index]
	if key != "--url" && key != "--remote" && key != "--agent" && key != "--target" {
		return false, nil
	}
	if *index+1 >= len(args) {
		return true, fmt.Errorf("%s requires a value", key)
	}
	value := args[*index+1]
	(*index)++
	switch key {
	case "--url":
		flags.url = value
	case "--remote":
		if !validRemoteName(value) {
			return true, fmt.Errorf("invalid remote %q: use letters, digits, dashes, and underscores", value)
		}
		flags.remote = value
	case "--agent":
		flags.agent = value
	case "--target":
		if err := requireTarget(value); err != nil {
			return true, err
		}
		flags.target = value
	}
	return true, nil
}

type mcpProbeTool struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"inputSchema"`
	OutputSchema json.RawMessage `json:"outputSchema,omitempty"`
}

var mcpProbeNonPublicPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("100:0:0:1::/64"),
	netip.MustParsePrefix("2001:2::/48"),
	netip.MustParsePrefix("2001:10::/28"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
	netip.MustParsePrefix("5f00::/16"),
}

type mcpProbeAuth struct {
	Status                    string   `json:"status"`
	DynamicClientRegistration string   `json:"dynamicClientRegistration"`
	LikelyAuthMode            string   `json:"likelyAuthMode"`
	UnavailableAuthModes      []string `json:"unavailableAuthModes,omitempty"`
	AuthorizationServers      []string `json:"authorizationServers,omitempty"`
	AuthorizationURL          string   `json:"authorizationURL,omitempty"`
	TokenURL                  string   `json:"tokenURL,omitempty"`
	RegistrationEndpoint      string   `json:"registrationEndpoint,omitempty"`
	ScopesSupported           []string `json:"scopesSupported,omitempty"`
	Message                   string   `json:"message"`
}

type mcpProbeResult struct {
	URL          string         `json:"url"`
	MCPStatus    string         `json:"mcpStatus"`
	MCPError     string         `json:"mcpError,omitempty"`
	Instructions string         `json:"instructions,omitempty"`
	Tools        []mcpProbeTool `json:"tools"`
	Auth         mcpProbeAuth   `json:"auth"`
}

func assessMCPAuth(ctx context.Context, httpClient *http.Client, rawURL string) mcpProbeAuth {
	discovery, err := mcp.DiscoverOAuthMetadata(ctx, httpClient, rawURL)
	if err != nil {
		return mcpProbeAuth{
			Status:                    "unknown",
			DynamicClientRegistration: "unknown",
			LikelyAuthMode:            "unknown",
			Message:                   "OAuth metadata could not be fully discovered; the authentication mode is unknown: " + err.Error(),
		}
	}

	scopes := discovery.ProtectedResource.ScopesSupported
	if len(scopes) == 0 {
		scopes = discovery.Metadata.ScopesSupported
	}
	auth := mcpProbeAuth{
		Status:               "oauth_metadata_discovered",
		AuthorizationServers: discovery.ProtectedResource.AuthorizationServers,
		AuthorizationURL:     discovery.Metadata.AuthorizationEndpoint,
		TokenURL:             discovery.Metadata.TokenEndpoint,
		ScopesSupported:      scopes,
	}
	if discovery.Metadata.RegistrationEndpoint == "" {
		auth.DynamicClientRegistration = "not_advertised"
		auth.LikelyAuthMode = "MCPAuthOAuth"
		auth.UnavailableAuthModes = []string{"MCPAuthOAuthDiscovery"}
		auth.Message = "OAuth endpoints were discovered, but the authorization server does not advertise a dynamic client registration endpoint. MCPAuthOAuthDiscovery cannot be used; MCPAuthOAuth is the likely mode."
		return auth
	}

	auth.DynamicClientRegistration = "advertised"
	auth.LikelyAuthMode = "MCPAuthOAuthDiscovery"
	auth.RegistrationEndpoint = discovery.Metadata.RegistrationEndpoint
	auth.Message = "OAuth and dynamic client registration are advertised. MCPAuthOAuthDiscovery is the likely mode; registration was not attempted."
	return auth
}

func newMCPProbeHTTPClient(serverURL *url.URL) *http.Client {
	allowLoopback := isMCPProbeLoopback(serverURL.Hostname())
	dialer := &net.Dialer{}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		resolved, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		var dialErr error
		for _, candidate := range resolved {
			if !mcpProbeAddressAllowed(candidate.IP, allowLoopback) {
				continue
			}
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(candidate.IP.String(), port))
			if err == nil {
				return conn, nil
			}
			dialErr = err
		}
		if dialErr != nil {
			return nil, dialErr
		}
		return nil, errors.New("OAuth metadata host does not resolve to an allowed address")
	}
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			if len(via) > 0 && !sameMCPProbeOrigin(via[0].URL, req.URL) {
				return errors.New("cross-origin OAuth metadata redirect is not allowed")
			}
			return nil
		},
	}
}

func mcpProbeAddressAllowed(ip net.IP, allowLoopback bool) bool {
	address, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	address = address.Unmap()
	if address.IsLoopback() {
		return allowLoopback
	}
	if !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLinkLocalUnicast() || address.IsLinkLocalMulticast() || address.IsMulticast() || address.IsUnspecified() {
		return false
	}
	for _, prefix := range mcpProbeNonPublicPrefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

func isMCPProbeLoopback(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func sameMCPProbeOrigin(left, right *url.URL) bool {
	return strings.EqualFold(left.Scheme, right.Scheme) && strings.EqualFold(left.Hostname(), right.Hostname()) && mcpProbePort(left) == mcpProbePort(right)
}

func mcpProbePort(u *url.URL) string {
	if u.Port() != "" {
		return u.Port()
	}
	if strings.EqualFold(u.Scheme, "https") {
		return "443"
	}
	return "80"
}

func printMCPProbe(result mcpProbeResult) error {
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(encoded))
	return nil
}

func probeMCP(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil || u.IsAbs() == false || (u.Scheme != "http" && u.Scheme != "https") {
		return errors.New("MCP URL must be an absolute HTTP or HTTPS URL")
	}
	if u.Scheme != "https" && !isMCPProbeLoopback(u.Hostname()) {
		return errors.New("MCP URL must use HTTPS except for loopback development")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	httpClient := newMCPProbeHTTPClient(u)
	authResult := make(chan mcpProbeAuth, 1)
	go func() {
		authResult <- assessMCPAuth(ctx, httpClient, rawURL)
	}()
	client := mcp.NewClient()
	defer client.DisconnectAll()
	if err := client.Connect(ctx, mcp.ServerConfig{Name: "probe", Transport: "http", URL: rawURL, HTTPClient: httpClient}); err != nil {
		var clientErr *mcp.MCPClientError
		if errors.As(err, &clientErr) && clientErr.StatusCode == http.StatusUnauthorized {
			return printMCPProbe(mcpProbeResult{
				URL:       rawURL,
				MCPStatus: "authentication_required",
				MCPError:  err.Error(),
				Tools:     make([]mcpProbeTool, 0),
				Auth:      <-authResult,
			})
		}
		cancel()
		<-authResult
		return err
	}
	result := mcpProbeResult{
		URL:          rawURL,
		MCPStatus:    "connected",
		Instructions: client.GetServerInstructions("probe"),
		Tools:        make([]mcpProbeTool, 0),
		Auth:         <-authResult,
	}
	tools, err := client.GetTools(ctx)
	if err != nil {
		return err
	}
	for _, item := range tools.Ordered(nil) {
		result.Tools = append(result.Tools, mcpProbeTool{
			Name: strings.TrimPrefix(item.Name, "probe_"), Description: item.Description, InputSchema: item.InputSchema, OutputSchema: item.OutputSchema,
		})
	}
	return printMCPProbe(result)
}
