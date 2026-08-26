package main

import (
	"bytes"
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
	"text/tabwriter"
	"time"

	airlockv1 "github.com/airlockrun/agentsdk/internal/airlockv1"
	"github.com/airlockrun/goai/mcp"
	"google.golang.org/protobuf/proto"
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
}

func resolveIntegrationTarget(ctx context.Context, flags integrationTargetFlags) (integrationTarget, error) {
	baseURL := os.Getenv("AIRLOCK_API_URL")
	agentID := os.Getenv("AIRLOCK_AGENT_ID")
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

	binding, ok, err := loadAgentBinding(".")
	if err != nil {
		return integrationTarget{}, err
	}
	if !ok {
		return integrationTarget{}, errors.New("workspace is not bound to Airlock; deploy or clone it first")
	}
	remoteName := flags.remote
	if remoteName == "" {
		remoteName = binding.DefaultRemote
	}
	remote, _ := binding.remote(remoteName)
	baseURL = normalizeBaseURL(flags.url)
	if baseURL != "" && remote.AirlockURL != "" && baseURL != normalizeBaseURL(remote.AirlockURL) {
		return integrationTarget{}, fmt.Errorf("remote %q is bound to %s, not %s; choose a different --remote name", remoteName, remote.AirlockURL, baseURL)
	}
	if baseURL == "" {
		baseURL = remote.AirlockURL
	}
	if baseURL == "" {
		return integrationTarget{}, fmt.Errorf("remote %q needs an Airlock URL: pass --url or configure %s", remoteName, agentBindingPath)
	}
	token, err = accessTokenForURL(ctx, baseURL)
	if err != nil {
		return integrationTarget{}, err
	}
	resolved, err := resolveAgentTarget(ctx, baseURL, token, flags.agent, remoteName, remote)
	if err != nil {
		return integrationTarget{}, err
	}
	return integrationTarget{baseURL: baseURL, agentID: resolved.AgentID, token: token}, nil
}

func (t integrationTarget) path(suffix string) string {
	if t.codegen {
		return "/api/codegen/integrations" + suffix
	}
	return "/api/v1/agents/" + url.PathEscape(t.agentID) + "/integrations" + suffix
}

func cmdIntegrations(args []string) error {
	if len(args) == 0 || args[0] != "list" {
		return errors.New("integrations requires: list [--remote <name>] [--url <url>] [--agent <slug-or-id>]")
	}
	flags, err := parseIntegrationTargetFlags(args[1:])
	if err != nil {
		return err
	}
	ctx := context.Background()
	target, err := resolveIntegrationTarget(ctx, flags)
	if err != nil {
		return err
	}
	var resp airlockv1.ListIntegrationsResponse
	if err := doProto(ctx, target.baseURL, http.MethodGet, target.path("/"), target.token, nil, &resp); err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "TYPE\tSLUG\tSTATUS\tDESCRIPTION")
	for _, item := range resp.Integrations {
		status := "not configured"
		if item.Configured {
			status = "configured"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", item.Type, item.Slug, status, item.Description)
	}
	return w.Flush()
}

func cmdConnection(args []string) error {
	if len(args) < 2 || args[0] != "request" {
		return errors.New("connection requires: request <slug> --path <path> [--method <method>] [--data <body>] [--header <name:value>]")
	}
	slug := args[1]
	method, path, body := http.MethodGet, "", ""
	headers := map[string]string{}
	var targetFlags integrationTargetFlags
	for i := 2; i < len(args); i++ {
		if handled, err := consumeIntegrationTargetFlag(args, &i, &targetFlags); handled || err != nil {
			if err != nil {
				return err
			}
			continue
		}
		switch args[i] {
		case "--method", "--path", "--data", "--header":
			if i+1 >= len(args) {
				return fmt.Errorf("%s requires a value", args[i])
			}
			key, value := args[i], args[i+1]
			i++
			switch key {
			case "--method":
				method = value
			case "--path":
				path = value
			case "--data":
				body = value
			case "--header":
				name, headerValue, ok := strings.Cut(value, ":")
				if !ok || strings.TrimSpace(name) == "" {
					return errors.New("--header must be name:value")
				}
				headers[strings.TrimSpace(name)] = strings.TrimSpace(headerValue)
			}
		default:
			return fmt.Errorf("unknown connection flag %q", args[i])
		}
	}
	if path == "" {
		return errors.New("--path is required")
	}
	ctx := context.Background()
	target, err := resolveIntegrationTarget(ctx, targetFlags)
	if err != nil {
		return err
	}
	var resp airlockv1.InvokeConnectionResponse
	err = doProto(ctx, target.baseURL, http.MethodPost, target.path("/connections/"+url.PathEscape(slug)+"/request"), target.token, &airlockv1.InvokeConnectionRequest{
		Method: method, Path: path, Body: []byte(body), Headers: headers,
	}, &resp)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "HTTP %d\n", resp.StatusCode)
	if _, err = os.Stdout.Write(resp.Body); err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return fmt.Errorf("upstream returned HTTP %d", resp.StatusCode)
	}
	return nil
}

func cmdMCP(args []string) error {
	if len(args) == 0 {
		return errors.New("mcp requires: probe, tools, or call")
	}
	switch args[0] {
	case "probe":
		if len(args) != 2 {
			return errors.New("mcp probe requires exactly one URL")
		}
		return probeMCP(args[1])
	case "tools":
		if len(args) < 2 {
			return errors.New("mcp tools requires a server slug")
		}
		flags, err := parseIntegrationTargetFlags(args[2:])
		if err != nil {
			return err
		}
		return listMCPTools(args[1], flags)
	case "call":
		if len(args) < 3 {
			return errors.New("mcp call requires: <server-slug> <tool> [--args <json>]")
		}
		arguments := "{}"
		var flags integrationTargetFlags
		for i := 3; i < len(args); i++ {
			if handled, err := consumeIntegrationTargetFlag(args, &i, &flags); handled || err != nil {
				if err != nil {
					return err
				}
				continue
			}
			if args[i] != "--args" || i+1 >= len(args) {
				return fmt.Errorf("unknown mcp call flag %q", args[i])
			}
			arguments = args[i+1]
			i++
		}
		if !json.Valid([]byte(arguments)) {
			return errors.New("MCP arguments must be valid JSON")
		}
		return callMCPTool(args[1], args[2], []byte(arguments), flags)
	default:
		return fmt.Errorf("unknown mcp subcommand %q", args[0])
	}
}

func listMCPTools(slug string, flags integrationTargetFlags) error {
	ctx := context.Background()
	target, err := resolveIntegrationTarget(ctx, flags)
	if err != nil {
		return err
	}
	var resp airlockv1.ListIntegrationMCPToolsResponse
	if err := doProto(ctx, target.baseURL, http.MethodGet, target.path("/mcp/"+url.PathEscape(slug)+"/tools"), target.token, nil, &resp); err != nil {
		return err
	}
	type listedTool struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"inputSchema"`
	}
	result := struct {
		Instructions string       `json:"instructions,omitempty"`
		Tools        []listedTool `json:"tools"`
	}{Instructions: resp.Instructions, Tools: make([]listedTool, len(resp.Tools))}
	for i, item := range resp.Tools {
		schema := json.RawMessage(item.InputSchemaJson)
		if len(schema) == 0 {
			schema = json.RawMessage(`{}`)
		}
		result.Tools[i] = listedTool{Name: item.Name, Description: item.Description, InputSchema: schema}
	}
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(encoded))
	return nil
}

func callMCPTool(slug, toolName string, arguments []byte, flags integrationTargetFlags) error {
	ctx := context.Background()
	target, err := resolveIntegrationTarget(ctx, flags)
	if err != nil {
		return err
	}
	var resp airlockv1.InvokeMCPToolResponse
	if err := doProto(ctx, target.baseURL, http.MethodPost, target.path("/mcp/"+url.PathEscape(slug)+"/call"), target.token, &airlockv1.InvokeMCPToolRequest{
		Tool: toolName, ArgumentsJson: arguments,
	}, &resp); err != nil {
		return err
	}
	if err := printProtoJSON(&resp); err != nil {
		return err
	}
	if resp.IsError {
		return errors.New("MCP tool returned an error")
	}
	return nil
}

func parseIntegrationTargetFlags(args []string) (integrationTargetFlags, error) {
	var flags integrationTargetFlags
	for i := 0; i < len(args); i++ {
		handled, err := consumeIntegrationTargetFlag(args, &i, &flags)
		if err != nil {
			return integrationTargetFlags{}, err
		}
		if !handled {
			return integrationTargetFlags{}, fmt.Errorf("unknown integration target flag %q", args[i])
		}
	}
	return flags, nil
}

func consumeIntegrationTargetFlag(args []string, index *int, flags *integrationTargetFlags) (bool, error) {
	key := args[*index]
	if key != "--url" && key != "--remote" && key != "--agent" {
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
	}
	return true, nil
}

type mcpProbeTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
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
	for _, item := range client.GetTools().Ordered(nil) {
		result.Tools = append(result.Tools, mcpProbeTool{
			Name: strings.TrimPrefix(item.Name, "probe_"), Description: item.Description, InputSchema: item.InputSchema,
		})
	}
	return printMCPProbe(result)
}

func printProtoJSON(message proto.Message) error {
	encoded, err := protoMarshal.Marshal(message)
	if err != nil {
		return err
	}
	var indented bytes.Buffer
	if err := json.Indent(&indented, encoded, "", "  "); err != nil {
		return err
	}
	fmt.Println(indented.String())
	return nil
}
