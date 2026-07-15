package agentsdk

import (
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/airlockrun/goai/tool"
	"github.com/robfig/cron/v3"
)

var (
	declarationSlugPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
	webhookPathPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	staticAssetNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	cronParser             = cron.NewParser(cron.SecondOptional | cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)
)

var frameworkRoutePatterns = []string{
	"POST /prompt",
	"POST /webhook/{name}",
	"POST /fire/{slug}",
	"POST /refresh",
	"GET /health",
	"POST /__air/tool/{name}",
	"GET /__air/assets/{name}",
	"GET /static/{name}",
}

func (a *Agent) beginRegistration(name string) func() {
	a.registrationM.Lock()
	if a.frozen {
		a.registrationM.Unlock()
		panic("agentsdk: " + name + ": registrations are frozen after Handler, Serve, or sync")
	}
	return a.registrationM.Unlock
}

// freeze establishes the immutable declaration boundary shared by Handler and
// sync. Registrations validate eagerly; this complete pass also catches invalid
// package-internal construction before any declaration is served or synced.
func (a *Agent) freeze() {
	a.registrationM.Lock()
	defer a.registrationM.Unlock()
	if a.frozen {
		return
	}
	a.validateRegistrations()
	a.frozen = true
}

func (a *Agent) validateRegistrations() {
	for _, t := range a.tools {
		validateRegisteredTool(t)
	}
	for _, w := range a.webhooks {
		validateWebhook(w)
	}
	for _, h := range a.scheduleHandlers {
		validateScheduleHandler(h)
	}
	validateRoutePatterns(a.routes, nil)
	for _, r := range a.routes {
		validateRoute(r)
	}
	for _, t := range a.topics {
		validateTopic(t)
	}
	for _, c := range a.auths {
		validateConnection(c)
	}
	for _, e := range a.envVars {
		validateEnvVar(e)
	}
	for _, d := range a.directories {
		validateDirectory(d)
	}
	for _, e := range a.execEndpoints {
		validateExecEndpoint(e)
	}
	for _, asset := range a.staticAssets {
		validateStaticAsset(asset)
	}
	for _, m := range a.mcps {
		validateMCP(m)
	}
	for _, i := range a.instructions {
		validateInstruction(i)
	}
	seenModels := make(map[string]struct{}, len(a.modelSlots))
	for _, slot := range a.modelSlots {
		validateModelSlot(slot)
		if _, ok := seenModels[slot.Slug]; ok {
			panic("agentsdk: duplicate RegisterModel: " + slot.Slug)
		}
		seenModels[slot.Slug] = struct{}{}
	}
}

func validateRegisteredTool(t *registeredTool) {
	if t == nil || strings.TrimSpace(t.Name) == "" {
		panic("agentsdk: RegisterTool: tool Name is required")
	}
	if strings.TrimSpace(t.Description) == "" {
		panic(fmt.Sprintf("agentsdk: RegisterTool(%q): tool Description is required", t.Name))
	}
	if t.Execute == nil && !t.IsProviderTool() {
		panic(fmt.Sprintf("agentsdk: RegisterTool(%q): tool Execute is required", t.Name))
	}
	validateAccess(fmt.Sprintf("RegisterTool(%q)", t.Name), t.access)
	validateJSON(fmt.Sprintf("RegisterTool(%q).InputSchema", t.Name), t.InputSchema)
	validateJSON(fmt.Sprintf("RegisterTool(%q).OutputSchema", t.Name), t.OutputSchema)
	for i, example := range t.InputExamples {
		validateJSON(fmt.Sprintf("RegisterTool(%q).InputExamples[%d]", t.Name, i), example.Input)
	}
}

func validateWebhook(w *Webhook) {
	if w == nil {
		panic("agentsdk: RegisterWebhook: nil *Webhook")
	}
	if !webhookPathPattern.MatchString(w.Path) {
		panic(fmt.Sprintf("agentsdk: RegisterWebhook(%q): Path must be one URL-safe path segment", w.Path))
	}
	if w.Handler == nil {
		panic(fmt.Sprintf("agentsdk: RegisterWebhook(%q): Handler is required", w.Path))
	}
	if strings.TrimSpace(w.Description) == "" {
		panic(fmt.Sprintf("agentsdk: RegisterWebhook(%q): Description is required", w.Path))
	}
	switch w.Verify {
	case "none", "token", "bearer":
		if w.Header != "" {
			panic(fmt.Sprintf("agentsdk: RegisterWebhook(%q): Header is not used with Verify %q", w.Path, w.Verify))
		}
	case "hmac":
		validateHeaderName(fmt.Sprintf("RegisterWebhook(%q).Header", w.Path), w.Header, true)
	case "ed25519":
		validateHeaderName(fmt.Sprintf("RegisterWebhook(%q).Header", w.Path), w.Header, false)
	default:
		panic(fmt.Sprintf("agentsdk: RegisterWebhook(%q): invalid Verify %q", w.Path, w.Verify))
	}
	validateTimeout(fmt.Sprintf("RegisterWebhook(%q).Timeout", w.Path), w.Timeout)
}

func validateScheduleHandler(h *scheduleHandler) {
	if h == nil {
		panic("agentsdk: schedule handler is nil")
	}
	validateSlug("schedule", h.slug)
	if h.handler == nil {
		panic(fmt.Sprintf("agentsdk: schedule handler %q: Handler is required", h.slug))
	}
	if strings.TrimSpace(h.description) == "" {
		panic(fmt.Sprintf("agentsdk: schedule handler %q: Description is required", h.slug))
	}
	validateTimeout(fmt.Sprintf("schedule handler %q Timeout", h.slug), h.timeout)
	switch h.kind {
	case "cron":
		if _, err := cronParser.Parse(h.recurrence); err != nil {
			panic(fmt.Sprintf("agentsdk: RegisterCron(%q): invalid Schedule: %v", h.slug, err))
		}
	case "schedule":
		if h.recurrence != "" {
			panic(fmt.Sprintf("agentsdk: RegisterSchedule(%q): recurrence must be empty", h.slug))
		}
	default:
		panic(fmt.Sprintf("agentsdk: schedule handler %q: invalid kind %q", h.slug, h.kind))
	}
}

func validateRoute(r *Route) {
	if r == nil {
		panic("agentsdk: RegisterRoute: nil *Route")
	}
	if !validHTTPMethod(r.Method) || r.Method != strings.ToUpper(r.Method) {
		panic(fmt.Sprintf("agentsdk: RegisterRoute(%q %q): Method must be an uppercase HTTP token", r.Method, r.Path))
	}
	if r.Path == "" || r.Path[0] != '/' || strings.ContainsAny(r.Path, "?#\r\n\t ") {
		panic(fmt.Sprintf("agentsdk: RegisterRoute(%s %q): Path must be an absolute HTTP path without a query or fragment", r.Method, r.Path))
	}
	segments := strings.Split(r.Path[1:], "/")
	for i, segment := range segments {
		decoded, err := url.PathUnescape(segment)
		if err != nil || decoded == "." || decoded == ".." || (segment == "" && i < len(segments)-1) {
			panic(fmt.Sprintf("agentsdk: RegisterRoute(%s %q): Path must not contain empty, dot, or dot-dot segments", r.Method, r.Path))
		}
	}
	if frameworkRoutePath(r.Path) {
		panic(fmt.Sprintf("agentsdk: RegisterRoute(%s %s): path is reserved by the framework", r.Method, r.Path))
	}
	if r.Handler == nil {
		panic(fmt.Sprintf("agentsdk: RegisterRoute(%s %s): Handler is required", r.Method, r.Path))
	}
	validateAccess(fmt.Sprintf("RegisterRoute(%s %s)", r.Method, r.Path), r.Access)
	if strings.TrimSpace(r.Description) == "" {
		panic(fmt.Sprintf("agentsdk: RegisterRoute(%s %s): Description is required", r.Method, r.Path))
	}
}

func validateStaticAsset(asset *StaticAsset) {
	if asset == nil {
		panic("agentsdk: RegisterStaticAsset: nil *StaticAsset")
	}
	if !staticAssetNamePattern.MatchString(asset.Name) {
		panic(fmt.Sprintf("agentsdk: RegisterStaticAsset(%q): Name must be one URL-safe path segment", asset.Name))
	}
	if asset.ContentType == "" || strings.TrimSpace(asset.ContentType) != asset.ContentType {
		panic(fmt.Sprintf("agentsdk: RegisterStaticAsset(%q): ContentType must be a valid media type", asset.Name))
	}
	if _, _, err := mime.ParseMediaType(asset.ContentType); err != nil {
		panic(fmt.Sprintf("agentsdk: RegisterStaticAsset(%q): ContentType must be a valid media type", asset.Name))
	}
	if asset.Data == nil {
		panic(fmt.Sprintf("agentsdk: RegisterStaticAsset(%q): Data is required", asset.Name))
	}
}

func validateRoutePatterns(routes map[string]*Route, candidate *Route) {
	mux := http.NewServeMux()
	noop := func(http.ResponseWriter, *http.Request) {}
	for _, pattern := range frameworkRoutePatterns {
		mux.HandleFunc(pattern, noop)
	}
	keys := make([]string, 0, len(routes))
	for key := range routes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		registerMuxPattern(mux, key, noop)
	}
	if candidate != nil {
		registerMuxPattern(mux, candidate.Method+" "+candidate.Path, noop)
	}
}

func registerMuxPattern(mux *http.ServeMux, pattern string, handler http.HandlerFunc) {
	defer func() {
		if recovered := recover(); recovered != nil {
			panic(fmt.Sprintf("agentsdk: RegisterRoute(%s): invalid or conflicting route: %v", pattern, recovered))
		}
	}()
	mux.HandleFunc(pattern, handler)
}

func frameworkRoutePath(path string) bool {
	if path == "/prompt" || path == "/webhook" || path == "/fire" || path == "/refresh" || path == "/health" || path == "/__air" || path == "/static" {
		return true
	}
	return strings.HasPrefix(path, "/webhook/") || strings.HasPrefix(path, "/fire/") || strings.HasPrefix(path, "/__air/") || strings.HasPrefix(path, "/static/")
}

func validateTopic(t *Topic) {
	if t == nil {
		panic("agentsdk: RegisterTopic: nil *Topic")
	}
	validateSlug("RegisterTopic", t.Slug)
	if strings.TrimSpace(t.Description) == "" {
		panic(fmt.Sprintf("agentsdk: RegisterTopic(%q): Description is required", t.Slug))
	}
	validateAccess(fmt.Sprintf("RegisterTopic(%q)", t.Slug), t.Access)
}

func validateConnection(c *Connection) {
	if c == nil {
		panic("agentsdk: RegisterConnection: nil *Connection")
	}
	validateSlug("RegisterConnection", c.Slug)
	if strings.TrimSpace(c.Name) == "" {
		panic(fmt.Sprintf("agentsdk: RegisterConnection(%q): Name is required", c.Slug))
	}
	if strings.TrimSpace(c.Description) == "" {
		panic(fmt.Sprintf("agentsdk: RegisterConnection(%q): Description is required", c.Slug))
	}
	validateHTTPURL(fmt.Sprintf("RegisterConnection(%q).BaseURL", c.Slug), c.BaseURL, true)
	switch c.AuthMode {
	case ConnectionAuthOAuth:
		validateHTTPURL(fmt.Sprintf("RegisterConnection(%q).AuthURL", c.Slug), c.AuthURL, true)
		validateHTTPURL(fmt.Sprintf("RegisterConnection(%q).TokenURL", c.Slug), c.TokenURL, true)
	case ConnectionAuthToken, ConnectionAuthNone:
		validateHTTPURL(fmt.Sprintf("RegisterConnection(%q).AuthURL", c.Slug), c.AuthURL, false)
		validateHTTPURL(fmt.Sprintf("RegisterConnection(%q).TokenURL", c.Slug), c.TokenURL, false)
	default:
		panic(fmt.Sprintf("agentsdk: RegisterConnection(%q): invalid AuthMode %q", c.Slug, c.AuthMode))
	}
	validateAuthInjection(fmt.Sprintf("RegisterConnection(%q)", c.Slug), c.AuthInjection)
	validateAccess(fmt.Sprintf("RegisterConnection(%q)", c.Slug), c.Access)
	for key := range c.Headers {
		validateHeaderName(fmt.Sprintf("RegisterConnection(%q).Headers key", c.Slug), key, true)
	}
	for key := range c.AuthParams {
		if strings.TrimSpace(key) == "" {
			panic(fmt.Sprintf("agentsdk: RegisterConnection(%q): AuthParams keys must not be empty", c.Slug))
		}
	}
}

func validateEnvVar(e *EnvVar) {
	if e == nil {
		panic("agentsdk: RegisterEnvVar: nil *EnvVar")
	}
	validateSlug("RegisterEnvVar", e.Slug)
	if strings.TrimSpace(e.Description) == "" {
		panic(fmt.Sprintf("agentsdk: RegisterEnvVar(%q): Description is required", e.Slug))
	}
	if e.Secret && e.Default != "" {
		panic("agentsdk: RegisterEnvVar(" + e.Slug + "): Default is not allowed for Secret=true")
	}
	if e.Pattern != "" {
		re, err := regexp.Compile(e.Pattern)
		if err != nil {
			panic("agentsdk: RegisterEnvVar(" + e.Slug + "): invalid Pattern: " + err.Error())
		}
		if e.Default != "" && !re.MatchString(e.Default) {
			panic("agentsdk: RegisterEnvVar(" + e.Slug + "): Default does not match Pattern")
		}
	}
}

func validateDirectory(d *Directory) {
	if d == nil {
		panic("agentsdk: directory declaration is nil")
	}
	if _, err := normalizePath(d.Path); err != nil {
		panic("agentsdk: RegisterDirectory: " + err.Error())
	}
	validateAccess(fmt.Sprintf("RegisterDirectory(%q).Read", d.Path), d.Read)
	validateAccess(fmt.Sprintf("RegisterDirectory(%q).Write", d.Path), d.Write)
	validateAccess(fmt.Sprintf("RegisterDirectory(%q).List", d.Path), d.List)
	if strings.TrimSpace(d.Description) == "" {
		panic(fmt.Sprintf("agentsdk: RegisterDirectory(%q): Description is required", d.Path))
	}
	if d.RetentionHours < 0 {
		panic(fmt.Sprintf("agentsdk: RegisterDirectory(%q): RetentionHours must not be negative", d.Path))
	}
	switch d.Scope {
	case ScopeNone, ScopeRun, ScopeConv, ScopeUser:
	default:
		panic(fmt.Sprintf("agentsdk: RegisterDirectory(%q): invalid Scope %q", d.Path, d.Scope))
	}
}

func validateExecEndpoint(e *ExecEndpoint) {
	if e == nil {
		panic("agentsdk: RegisterExecEndpoint: nil *ExecEndpoint")
	}
	validateSlug("RegisterExecEndpoint", e.Slug)
	if strings.TrimSpace(e.Description) == "" {
		panic("agentsdk: RegisterExecEndpoint(" + e.Slug + "): Description is required")
	}
	validateAccess(fmt.Sprintf("RegisterExecEndpoint(%q)", e.Slug), e.Access)
	if e.Access == AccessPublic {
		panic("agentsdk: RegisterExecEndpoint(" + e.Slug + "): AccessPublic is not allowed")
	}
}

func validateMCP(m *MCP) {
	if m == nil {
		panic("agentsdk: RegisterMCP: nil *MCP")
	}
	validateSlug("RegisterMCP", m.Slug)
	if strings.TrimSpace(m.Name) == "" {
		panic(fmt.Sprintf("agentsdk: RegisterMCP(%q): Name is required", m.Slug))
	}
	validateHTTPURL(fmt.Sprintf("RegisterMCP(%q).URL", m.Slug), m.URL, true)
	switch m.AuthMode {
	case MCPAuthOAuth:
		validateHTTPURL(fmt.Sprintf("RegisterMCP(%q).AuthURL", m.Slug), m.AuthURL, true)
		validateHTTPURL(fmt.Sprintf("RegisterMCP(%q).TokenURL", m.Slug), m.TokenURL, true)
	case MCPAuthOAuthDiscovery, MCPAuthToken, MCPAuthNone:
		validateHTTPURL(fmt.Sprintf("RegisterMCP(%q).AuthURL", m.Slug), m.AuthURL, false)
		validateHTTPURL(fmt.Sprintf("RegisterMCP(%q).TokenURL", m.Slug), m.TokenURL, false)
	default:
		panic(fmt.Sprintf("agentsdk: RegisterMCP(%q): invalid AuthMode %q", m.Slug, m.AuthMode))
	}
	validateAuthInjection(fmt.Sprintf("RegisterMCP(%q)", m.Slug), m.AuthInjection)
	validateAccess(fmt.Sprintf("RegisterMCP(%q)", m.Slug), m.Access)
}

func validateInstruction(i *Instruction) {
	if i == nil {
		panic("agentsdk: AddInstruction: nil *Instruction")
	}
	if strings.TrimSpace(i.Text) == "" {
		panic("agentsdk: AddInstruction: Text is required")
	}
	seen := make(map[Access]struct{}, len(i.Access))
	for _, access := range i.Access {
		validateAccess("AddInstruction", access)
		if _, ok := seen[access]; ok {
			panic(fmt.Sprintf("agentsdk: AddInstruction: duplicate Access %q", access))
		}
		seen[access] = struct{}{}
	}
}

func validateModelSlot(slot *ModelSlot) {
	if slot == nil {
		panic("agentsdk: RegisterModel: nil *ModelSlot")
	}
	validateSlug("RegisterModel", slot.Slug)
	switch slot.Capability {
	case CapText, CapVision, CapEmbedding, CapImage, CapSpeech, CapTranscription, CapSearch:
	default:
		panic(fmt.Sprintf("agentsdk: RegisterModel(%q): invalid Capability %q", slot.Slug, slot.Capability))
	}
	if strings.TrimSpace(slot.Description) == "" {
		panic(fmt.Sprintf("agentsdk: RegisterModel(%q): Description is required", slot.Slug))
	}
}

func validateAccess(context string, access Access) {
	switch access {
	case AccessAdmin, AccessUser, AccessPublic:
		return
	default:
		if access == "" {
			panic("agentsdk: " + context + ": Access is required")
		}
		panic(fmt.Sprintf("agentsdk: %s: invalid Access %q", context, access))
	}
}

func validateSlug(context, slug string) {
	if !declarationSlugPattern.MatchString(slug) {
		panic(fmt.Sprintf("agentsdk: %s(%q): Slug must start with a lowercase letter and contain only lowercase letters, digits, underscores, or dashes", context, slug))
	}
}

func validateTimeout(context string, timeout time.Duration) {
	if timeout < 0 {
		panic("agentsdk: " + context + " must not be negative")
	}
}

func validateHTTPURL(context, raw string, required bool) {
	if raw == "" {
		if required {
			panic("agentsdk: " + context + " is required")
		}
		return
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Fragment != "" {
		panic(fmt.Sprintf("agentsdk: %s must be an absolute http(s) URL without userinfo or fragment", context))
	}
}

func validateAuthInjection(context string, injection AuthInjection) {
	switch injection.Type {
	case "", AuthInjectBearer, AuthInjectAPIKey, AuthInjectPathPrefix:
	case AuthInjectQueryParam:
		if strings.TrimSpace(injection.Name) == "" {
			panic("agentsdk: " + context + ": AuthInjection.Name is required for query_param")
		}
	default:
		panic(fmt.Sprintf("agentsdk: %s: invalid AuthInjection.Type %q", context, injection.Type))
	}
}

func validateHeaderName(context, name string, required bool) {
	if name == "" {
		if required {
			panic("agentsdk: " + context + " is required")
		}
		return
	}
	if http.CanonicalHeaderKey(name) == "" {
		panic(fmt.Sprintf("agentsdk: %s contains an invalid HTTP header name %q", context, name))
	}
}

func validateJSON(context string, raw json.RawMessage) {
	if len(raw) > 0 && !json.Valid(raw) {
		panic("agentsdk: " + context + " must be valid JSON")
	}
}

func validHTTPMethod(method string) bool {
	if method == "" {
		return false
	}
	for _, c := range []byte(method) {
		if c <= 0x20 || c >= 0x7f || strings.ContainsRune(`()<>@,;:\"/[]?={}`, rune(c)) {
			return false
		}
	}
	return true
}

func cloneTool(t tool.Tool) tool.Tool {
	t.Args = append(json.RawMessage(nil), t.Args...)
	t.InputSchema = append(json.RawMessage(nil), t.InputSchema...)
	t.OutputSchema = append(json.RawMessage(nil), t.OutputSchema...)
	t.InputExamples = append([]tool.ToolInputExample(nil), t.InputExamples...)
	for i := range t.InputExamples {
		t.InputExamples[i].Input = append(json.RawMessage(nil), t.InputExamples[i].Input...)
	}
	if t.ProviderOptions != nil {
		t.ProviderOptions = cloneAnyMap(t.ProviderOptions)
	}
	return t
}

func cloneAnyMap(src map[string]any) map[string]any {
	dst := make(map[string]any, len(src))
	for key, value := range src {
		dst[key] = cloneAny(value)
	}
	return dst
}

func cloneAny(value any) any {
	switch value := value.(type) {
	case map[string]any:
		return cloneAnyMap(value)
	case []any:
		copyValue := make([]any, len(value))
		for i := range value {
			copyValue[i] = cloneAny(value[i])
		}
		return copyValue
	case map[string]string:
		return cloneStringMap(value)
	case []string:
		return append([]string(nil), value...)
	case json.RawMessage:
		return append(json.RawMessage(nil), value...)
	case []byte:
		return append([]byte(nil), value...)
	default:
		return value
	}
}

func cloneStringMap(src map[string]string) map[string]string {
	if src == nil {
		return nil
	}
	dst := make(map[string]string, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}
