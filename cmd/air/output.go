package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"unicode/utf8"

	"golang.org/x/mod/modfile"

	"github.com/airlockrun/agentsdk/remote"
	"github.com/airlockrun/agentsdk/wire"

	"google.golang.org/protobuf/proto"
)

type cliAction struct {
	Kind       string   `json:"kind"`
	Message    string   `json:"message"`
	Executable string   `json:"executable"`
	Args       []string `json:"args"`
}

type cliError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	ExitCode   int    `json:"exitCode"`
	ApprovalID string `json:"approvalId,omitempty"`
}

type cliWarning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type redactedCLIError struct {
	cause  error
	secret string
}

func (e *redactedCLIError) Error() string {
	if e.secret == "" {
		return e.cause.Error()
	}
	return strings.ReplaceAll(e.cause.Error(), e.secret, "[redacted]")
}

func (e *redactedCLIError) Unwrap() error { return e.cause }

type cliOperation struct {
	Kind                string `json:"kind"`
	Admission           string `json:"admission"`
	Action              string `json:"action,omitempty"`
	Slug                string `json:"slug,omitempty"`
	AppID               string `json:"appId,omitempty"`
	RequestedSlug       string `json:"requestedSlug,omitempty"`
	RequestedName       string `json:"requestedName,omitempty"`
	RuntimeID           string `json:"runtimeId,omitempty"`
	Target              string `json:"target,omitempty"`
	RequestID           string `json:"requestId,omitempty"`
	HostID              string `json:"hostId,omitempty"`
	ConnectorID         string `json:"connectorId,omitempty"`
	JobID               string `json:"jobId,omitempty"`
	TaskID              string `json:"taskId,omitempty"`
	RevisionID          string `json:"revisionId,omitempty"`
	Generation          string `json:"generation,omitempty"`
	ExpectedSourceETag  string `json:"expectedSourceETag,omitempty"`
	UploadedSourceState string `json:"uploadedSourceState,omitempty"`
	Definition          string `json:"definition,omitempty"`
	SessionID           string `json:"sessionId,omitempty"`
	RunID               string `json:"runId,omitempty"`
	TestRunID           string `json:"testRunId,omitempty"`
	Suite               string `json:"suite,omitempty"`
}

type toolInvocationResult struct {
	RunID  string          `json:"runId"`
	Result json.RawMessage `json:"result"`
}

type cliResult struct {
	sourceStdout  *os.File
	SchemaVersion int           `json:"schemaVersion"`
	Command       string        `json:"command"`
	Status        string        `json:"status"`
	Data          any           `json:"data"`
	Operation     *cliOperation `json:"operation,omitempty"`
	Configuration any           `json:"configuration,omitempty"`
	Context       any           `json:"context,omitempty"`
	Error         *cliError     `json:"error,omitempty"`
	Guidance      []cliAction   `json:"guidance,omitempty"`
	Warnings      []cliWarning  `json:"warnings,omitempty"`
}

// CLI invocations execute serially in one process. This sink owns only the
// current output document; it is not credential, selection or operation state.
var invocationOutput *cliResult
var humanOutput bool

func setResult(data any, status string) {
	if invocationOutput == nil {
		return
	}
	invocationOutput.Data = data
	invocationOutput.Status = status
}

func writeJSONData(value any) error {
	if invocationOutput != nil {
		raw, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("encode command data: %w", err)
		}
		setResult(json.RawMessage(raw), "completed")
		return nil
	}
	if humanOutput {
		switch result := value.(type) {
		case toolInvocationResult:
			fmt.Println("Tool result")
			value, err := decodeJSONValue(result.Result)
			if err != nil {
				return err
			}
			renderHumanData(os.Stdout, value, "")
			return nil
		case *wire.AgentRunInfo:
			fmt.Printf("%s — %s\n", safeTableCell(result.Definition), safeTableCell(result.Status))
			if result.Status != "completed" {
				fmt.Printf("Run: %s\n", result.ID)
			}
			if result.Error != "" {
				fmt.Printf("Error: %s\n", safeTableCell(result.Error))
			}
			if result.Reply != nil {
				if result.Reply.Question != "" {
					fmt.Println(safeTableCell(result.Reply.Question))
					if result.SessionID != "" {
						fmt.Printf("Session: %s\n", result.SessionID)
					}
				}
				if len(result.Reply.Output) > 0 {
					output, err := decodeJSONValue(result.Reply.Output)
					if err != nil {
						return err
					}
					renderHumanData(os.Stdout, output, "")
				}
				for _, artifact := range result.Reply.Artifacts {
					fmt.Printf("Artifact: %s (%s, %d bytes)\n", safeTableCell(artifact.Path), safeTableCell(artifact.ContentType), artifact.Size)
				}
			}
			return nil
		case []wire.AgentRunInfo:
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "DEFINITION\tSTATUS\tUPDATED\tRUN ID")
			for _, row := range result {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", safeTableCell(row.Definition), safeTableCell(row.Status), row.UpdatedAt.Format("2006-01-02 15:04:05Z07:00"), row.ID)
			}
			return w.Flush()
		}
		body, err := json.Marshal(value)
		if err != nil {
			return err
		}
		data, err := decodeJSONValue(body)
		if err != nil {
			return err
		}
		renderHumanData(os.Stdout, data, "")
		return nil
	}
	return json.NewEncoder(os.Stdout).Encode(value)
}

func decodeJSONValue(raw []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err != nil {
			return nil, err
		}
		return nil, errors.New("expected one JSON value")
	}
	return value, nil
}

func renderHumanData(w io.Writer, value any, indent string) {
	switch data := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(data))
		for key := range data {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			v := data[key]
			if v == nil || v == "" {
				continue
			}
			switch v.(type) {
			case map[string]any, []any:
				fmt.Fprintf(w, "%s%s:\n", indent, safeTableCell(key))
				renderHumanData(w, v, indent+"  ")
			default:
				fmt.Fprintf(w, "%s%s: %s\n", indent, safeTableCell(key), safeTableCell(fmt.Sprint(v)))
			}
		}
	case []any:
		if len(data) == 0 {
			fmt.Fprintln(w, indent+"None")
		}
		for i, item := range data {
			if i > 0 {
				fmt.Fprintln(w)
			}
			renderHumanData(w, item, indent)
		}
	default:
		fmt.Fprintln(w, indent+safeTableCell(fmt.Sprint(value)))
	}
}

func writeProtoData(value proto.Message) error {
	body, err := protoMarshal.Marshal(value)
	if err != nil {
		return err
	}
	return writeJSONData(json.RawMessage(body))
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var approval *approvalRequiredError
	if errors.As(err, &approval) {
		return 77
	}
	var remoteExit *hostCommandExitError
	if errors.As(err, &remoteExit) {
		return remoteExit.code
	}
	return 1
}

func jsonRequested(args []string) bool {
	_, requested := extractJSONOption(args)
	return requested
}

func extractJSONOption(args []string) ([]string, bool) {
	var prefix []string
	for _, arg := range args {
		if arg == "--json" {
			continue
		}
		if strings.HasPrefix(arg, "-") {
			break
		}
		prefix = append(prefix, arg)
	}
	command, _, known := findPublicCommand(prefix)
	values := map[string]bool{}
	if known {
		for _, flag := range declaredFlags(command) {
			values[flag.Name] = flag.TakesValue
		}
	}
	var clean []string
	requested := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return append(clean, args[i:]...), requested
		}
		if values[arg] && i+1 < len(args) {
			clean = append(clean, arg, args[i+1])
			i++
			continue
		}
		if arg == "--json" {
			requested = true
			continue
		}
		clean = append(clean, arg)
	}
	return clean, requested
}

// invokeCLI owns the one-document machine boundary. Existing progress writers
// and child processes use stderr in machine mode, never the result stream.
func invokeCLI(args []string, stdout *os.File) error {
	if !jsonRequested(args) {
		humanOutput = true
		defer func() { humanOutput = false }()
		return run(args)
	}
	clean, _ := extractJSONOption(args)
	selectedCommand, _, _ := findPublicCommand(clean)
	command := selectedCommand.Command
	if command == "" && len(clean) > 0 {
		command = clean[0]
	}
	if command == "" {
		command = "air"
	}
	result := &cliResult{SchemaVersion: 1, Command: command, Status: "completed", Data: map[string]any{}, sourceStdout: stdout}
	invocationOutput = result
	previous := os.Stdout
	os.Stdout = os.Stderr
	defer func() { os.Stdout = previous; invocationOutput = nil }()
	var err error
	if path, help := helpRequest(clean); help {
		// Help is local discovery, not a wrapped human transcript.
		invocationOutput = nil
		err = printHelp(stdout, path, true)
		if err == nil {
			return nil
		}
		invocationOutput = result
		result.Command = "help"
	}
	if len(clean) == 1 && (clean[0] == "version" || clean[0] == "--version") {
		setResult(map[string]string{"version": strings.TrimPrefix(runningAgentSDKVersion(), "v")}, "completed")
	} else if err == nil {
		err = run(clean)
	}
	if err != nil {
		code := "command_failed"
		if result.Status == "pending" && result.Command == "login" {
			code = "authorization_pending"
		} else {
			result.Status = "failed"
		}
		if result.Operation != nil && result.Operation.Admission == "unknown" {
			code = "effect_unknown"
		}
		var statusError *httpStatusError
		statusCode := 0
		if errors.As(err, &statusError) {
			statusCode = statusError.StatusCode
		}
		var remoteStatus *remote.HTTPError
		if errors.As(err, &remoteStatus) {
			statusCode = remoteStatus.StatusCode
		}
		if statusCode == 401 {
			code = "authentication_required"
			if result.Operation != nil && result.Operation.Admission == "accepted" {
				code = "observation_authentication_required"
			}
		}
		if statusCode == 403 {
			code = "permission_denied"
			if result.Operation != nil && result.Operation.Admission == "accepted" {
				code = "observation_permission_denied"
			}
		}
		var stale *staleSourceError
		if errors.As(err, &stale) {
			code = "source_conflict"
		}
		var deploymentFailure *deploymentTaskFailure
		if errors.As(err, &deploymentFailure) {
			code = "deployment_failed"
		}
		var loginError *loginExpiredError
		if errors.As(err, &loginError) {
			code = "authentication_required"
			if result.Operation != nil && result.Operation.Admission == "accepted" {
				code = "observation_authentication_required"
			}
		}
		if errors.Is(err, context.DeadlineExceeded) {
			if result.Operation != nil && result.Operation.Admission == "unknown" {
				code = "effect_unknown"
			} else {
				code, result.Status = "command_timeout", "timed_out"
				if containsArgument(clean, "--wait") || command == "agent wait" || command == "status" || command == "inspect" || command == "deploy status" || result.Operation != nil && result.Operation.Admission == "accepted" {
					code = "observation_timeout"
				}
			}
		}
		if errors.Is(err, context.Canceled) {
			code = "observation_interrupted"
		}
		if exitCode(err) == 77 {
			code, result.Status = "approval_required", "pending"
		}
		result.Error = &cliError{Code: code, Message: err.Error(), ExitCode: exitCode(err)}
		var approval *approvalRequiredError
		if errors.As(err, &approval) {
			result.Error.ApprovalID = approval.id
		}
		if len(result.Guidance) == 0 {
			topic := result.Command
			if !knownHelp(topic) {
				topic = ""
			}
			if action := localHelpAction(topic); action.Executable != "" {
				result.Guidance = []cliAction{action}
			}
		}
	}
	if encodeErr := json.NewEncoder(stdout).Encode(result); encodeErr != nil {
		return errors.Join(err, encodeErr)
	}
	return err
}

func cliOriginalStdout() *os.File {
	if invocationOutput == nil {
		return os.Stdout
	}
	if invocationOutput.sourceStdout == nil {
		panic("CLI output requires its original stdout")
	}
	return invocationOutput.sourceStdout
}

func localHelpAction(topic string) cliAction {
	executable, err := os.Executable()
	if err != nil {
		return cliAction{}
	}
	cwd, err := os.Getwd()
	if err != nil {
		return cliAction{}
	}
	return helpActionFor(executable, cwd, topic)
}

// A module-owned tool is reproducible after Go removes its invocation binary.
// Outside such a module, offer only a verified persistent executable.
func helpActionFor(executable, cwd, topic string) cliAction {
	action := cliAction{Kind: "help", Message: "Read local command usage without authentication or network access"}
	for dir := cwd; ; dir = filepath.Dir(dir) {
		body, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil {
			mf, err := modfile.Parse("go.mod", body, nil)
			if err == nil {
				for _, tool := range mf.Tool {
					if tool.Path == "github.com/airlockrun/agentsdk/cmd/air" {
						action.Executable, action.Args = "go", []string{"-C", cwd, "tool", "air"}
						break
					}
				}
			}
			break
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	if action.Executable == "" {
		clean := filepath.ToSlash(executable)
		if strings.Contains(clean, "/go-build") || strings.Contains(clean, "/.cache/") || strings.HasSuffix(clean, ".test") {
			return cliAction{}
		}
		info, err := os.Stat(executable)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&0111 == 0 {
			return cliAction{}
		}
		action.Executable = executable
	}
	action.Args = append(action.Args, "help")
	action.Args = append(action.Args, strings.Fields(topic)...)
	return action
}

// Call only at a failed mutation admission, never at a later inspection/wait.
func admissionFailure(err error) error {
	if invocationOutput == nil || invocationOutput.Operation == nil || invocationOutput.Operation.Admission != "unknown" {
		return err
	}
	status := 0
	var httpErr *httpStatusError
	if errors.As(err, &httpErr) {
		status = httpErr.StatusCode
	}
	var remoteErr *remote.HTTPError
	if errors.As(err, &remoteErr) {
		var request interface{ RequestMethod() string }
		if !errors.As(err, &request) {
			return err
		}
		switch request.RequestMethod() {
		case "POST", "PUT", "PATCH", "DELETE":
		default:
			return err
		}
		status = remoteErr.StatusCode
	}
	var approval *approvalRequiredError
	var stale *staleSourceError
	if status == 401 || status == 403 || errors.As(err, &approval) {
		invocationOutput.Operation.Admission = "blocked"
		if approval != nil {
			invocationOutput.Guidance = append([]cliAction{{Kind: "approval", Message: "Resolve the pending human confirmation in the developer conversation; the protected operation was not executed. Do not replay the whole shell command."}}, invocationOutput.Guidance...)
		}
	} else if errors.As(err, &stale) {
		invocationOutput.Operation.Admission = "rejected"
	}
	return err
}

func containsArgument(args []string, value string) bool {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		if optionTakesValue(arg) {
			i++
			continue
		}
		if arg == value {
			return true
		}
	}
	return false
}

func selectedAction(s selectedDeployment, command, message string, extra ...string) cliAction {
	var args []string
	if s.flags.dir != "" && s.flags.dir != "." {
		args = append(args, "-C", s.flags.dir)
	}
	args = append(args, "tool", "air")
	args = append(args, strings.Fields(command)...)
	args = append(args, "--remote", s.remoteName, "--agent", s.app.AgentID, "--target", s.info.Name)
	if s.flags.url != "" {
		args = append(args, "--url", s.baseURL)
	}
	args = append(args, extra...)
	return cliAction{Kind: "observe", Message: message, Executable: "go", Args: args}
}

// Human hints describe the validated local context; machine actions retain exact
// app/target coordinates independently of mutable local selection.
func humanSelectedAction(s selectedDeployment, command string, extra ...string) cliAction {
	action := selectedAction(s, command, "", extra...)
	binding, _, err := loadAgentBinding(s.flags.dir)
	if err != nil {
		return action
	}
	bound, ok := binding.remote(s.remoteName)
	exactBinding := ok && bound.AgentID == s.app.AgentID && normalizeBaseURL(bound.AirlockURL) == s.baseURL
	target := s.info.Name
	ref := bound.Previews[s.app.AgentID]
	if exactBinding && s.info.Kind == "personal_preview" && ref.DeploymentID == s.info.Id && ref.Name == s.info.Name {
		target = "preview"
	}
	var args []string
	if s.flags.dir != "" && s.flags.dir != "." {
		args = append(args, "-C", s.flags.dir)
	}
	args = append(args, "tool", "air")
	args = append(args, strings.Fields(command)...)
	args = append(args, "--remote", s.remoteName)
	if !exactBinding {
		app := s.app.AgentID
		if s.app.Slug != "" {
			app = s.app.Slug
		}
		args = append(args, "--agent", app)
	}
	args = append(args, "--target", target)
	if s.flags.url != "" {
		args = append(args, "--url", s.baseURL)
	}
	action.Args = append(args, extra...)
	return action
}

func shellCommand(action cliAction) string {
	words := append([]string{action.Executable}, action.Args...)
	for i, word := range words {
		if word == "" || strings.ContainsAny(word, " \t\r\n'\"\\$`;|&()<>*?![]{}#~") {
			words[i] = "'" + strings.ReplaceAll(word, "'", "'\"'\"'") + "'"
		}
	}
	return strings.Join(words, " ")
}

func setSelectedContext(s selectedDeployment) {
	if invocationOutput == nil {
		return
	}
	context := map[string]string{"directory": s.flags.dir, "remote": s.remoteName, "appId": s.app.AgentID, "app": s.app.Slug, "displayName": s.app.displayName, "target": s.info.Name, "runtimeId": s.info.Id}
	if s.expectedRevision != "" {
		context["expectedRevisionId"] = s.expectedRevision
	}
	invocationOutput.Context = context
	invocationOutput.Guidance = []cliAction{selectedAction(s, "inspect", "Inspect the same deployment")}
}

type responseBody struct {
	StatusCode  int    `json:"statusCode,omitempty"`
	ContentType string `json:"contentType,omitempty"`
	Encoding    string `json:"encoding"`
	Body        string `json:"body"`
}

func encodedBody(body []byte, status int, contentType string) responseBody {
	out := responseBody{StatusCode: status, ContentType: contentType, Encoding: "utf-8", Body: string(body)}
	mediaType, _, _ := mime.ParseMediaType(contentType)
	text := mediaType == "" || strings.HasPrefix(mediaType, "text/") || strings.Contains(mediaType, "json") || strings.Contains(mediaType, "xml") || mediaType == "application/javascript" || mediaType == "application/x-www-form-urlencoded"
	if !utf8.Valid(body) || !text {
		out.Encoding, out.Body = "base64", base64.StdEncoding.EncodeToString(body)
	}
	return out
}

func writeResponseBody(reader io.Reader, status int, contentType string, machine bool) error {
	if !machine {
		_, err := io.Copy(os.Stdout, reader)
		return err
	}
	const limit = 16 << 20
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return err
	}
	if len(body) > limit {
		return fmt.Errorf("response exceeds JSON body limit of %d bytes; use raw output", limit)
	}
	return writeJSONData(encodedBody(body, status, contentType))
}
