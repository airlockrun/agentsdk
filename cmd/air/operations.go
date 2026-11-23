package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"time"

	airlockv1 "github.com/airlockrun/agentsdk/internal/airlockv1"
	"github.com/airlockrun/agentsdk/remote"
	"google.golang.org/protobuf/types/known/structpb"
)

func (s selectedDeployment) client(revision string) (*remote.Client, error) {
	if revision == "" {
		revision = s.info.SourceRevisionId
	}
	return remote.New(remote.Config{URL: s.baseURL, AppID: s.app.AgentID, Target: s.info.Name, Revision: revision, HTTPClient: apiClient,
		Token: func(ctx context.Context) (string, error) { return accessTokenForURL(ctx, s.baseURL) }})
}

func cmdOperation(command string, args []string) error {
	input, data, requestID, contractHash, revision, prompt := "{}", "", "", "", "", ""
	headers := http.Header{}
	var selectionArgs []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--input", "--data", "--header", "--request-id", "--contract", "--revision", "--prompt":
			key := args[i]
			if i+1 >= len(args) {
				return fmt.Errorf("%s requires a value", key)
			}
			i++
			switch key {
			case "--prompt":
				prompt = args[i]
			case "--input":
				input = args[i]
			case "--data":
				data = args[i]
			case "--request-id":
				requestID = args[i]
			case "--contract":
				contractHash = args[i]
			case "--revision":
				revision = args[i]
			case "--header":
				name, value, ok := strings.Cut(args[i], ":")
				if !ok || strings.TrimSpace(name) == "" || strings.ContainsAny(name+value, "\r\n") || strings.EqualFold(strings.TrimSpace(name), "Authorization") {
					return errors.New("--header requires name:value and cannot override authentication")
				}
				headers.Add(strings.TrimSpace(name), strings.TrimSpace(value))
			}
		default:
			selectionArgs = append(selectionArgs, args[i])
		}
	}
	f, positional, err := parseTargetFlags(selectionArgs)
	if err != nil {
		return err
	}
	if f.wait && (command != "agent" || len(positional) == 0 || positional[0] != "start") {
		return errors.New("--wait on app operations requires agent start; use agent wait for an existing run")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()
	s, err := resolveDeployment(ctx, f, false)
	if err != nil {
		return err
	}
	if revision != "" && revision != s.info.SourceRevisionId {
		return errors.New("selected deployment source revision changed")
	}
	setSelectedContext(s)
	fmt.Fprintln(os.Stderr, s.heading())
	client, err := s.client(revision)
	if err != nil {
		return err
	}
	switch command {
	case "request":
		if len(positional) != 2 {
			return errors.New("request requires METHOD /path")
		}
		response, err := client.Request(ctx, positional[0], positional[1], strings.NewReader(data), headers)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		fmt.Fprintf(os.Stderr, "HTTP %d (%s/%s)\n", response.StatusCode, s.remoteName, s.info.Name)
		if err := writeResponseBody(response.Body, response.StatusCode, response.Header.Get("Content-Type"), f.json); err != nil {
			return err
		}
		if response.StatusCode >= 400 {
			return fmt.Errorf("app returned HTTP %d", response.StatusCode)
		}
		return nil
	case "tools":
		if len(positional) == 1 && positional[0] == "list" {
			var out airlockv1.ListToolsResponse
			if err := doProto(ctx, s.baseURL, http.MethodGet, s.path("/tools"), s.token, nil, &out); err != nil {
				return err
			}
			return writeProtoJSON(&out)
		}
		if len(positional) != 2 || positional[0] != "invoke" {
			return errors.New("tools requires list or invoke <name> --input <json>")
		}
		value := &structpb.Value{}
		if err := value.UnmarshalJSON([]byte(input)); err != nil {
			return err
		}
		if invocationOutput != nil {
			invocationOutput.Operation = &cliOperation{Kind: "tool_invoke", Admission: "unknown", Action: positional[1], RuntimeID: s.info.Id, Target: s.info.Name, RevisionID: s.info.SourceRevisionId}
		}
		out, err := client.InvokeTool(ctx, positional[1], value)
		if err != nil {
			return admissionFailure(err)
		}
		if invocationOutput != nil {
			invocationOutput.Operation.Admission = "accepted"
			invocationOutput.Operation.RunID = out.RunId
			setResult(map[string]string{"runId": out.RunId}, "completed")
		}
		if !deployUUIDRe.MatchString(out.RunId) {
			return errors.New("tool response did not include a valid run ID; inspect before invoking again")
		}
		if invocationOutput != nil {
			invocationOutput.Guidance = []cliAction{selectedAction(s, "logs", "Inspect this exact tool run", out.RunId)}
		}
		if !json.Valid([]byte(out.ResultJson)) {
			return errors.New("tool returned invalid result JSON; inspect its run before invoking again")
		}
		return writeJSONData(toolInvocationResult{RunID: out.RunId, Result: json.RawMessage(out.ResultJson)})
	case "agent":
		if len(positional) < 2 {
			return errors.New("agent requires start|list <definition>, get|wait|cancel <definition> <run-id>, or continue <definition> <session-id> --prompt <text>")
		}
		action, definition := positional[0], positional[1]
		if action == "list" {
			if len(positional) != 2 {
				return errors.New("agent list requires one definition")
			}
			runs, err := client.ListAgents(ctx, definition)
			if err != nil {
				return err
			}
			return writeJSONData(runs)
		}
		if action == "continue" {
			if len(positional) != 3 || !deployUUIDRe.MatchString(positional[2]) || strings.TrimSpace(prompt) == "" {
				return errors.New("agent continue requires definition, session UUID and --prompt")
			}
			if requestID == "" {
				requestID, err = newUUID()
				if err != nil {
					return err
				}
			}
			if !deployUUIDRe.MatchString(requestID) {
				return errors.New("--request-id must be a UUID")
			}
			fmt.Fprintf(os.Stderr, "Request ID: %s\n", requestID)
			if invocationOutput != nil {
				invocationOutput.Operation = &cliOperation{Kind: "agent_continue", Admission: "unknown", RequestID: requestID, Definition: definition, SessionID: positional[2]}
				invocationOutput.Guidance = []cliAction{selectedAction(s, "agent list", "Inspect runs without replay; preserve this request ID and exact payload", definition)}
			}
			run, err := client.ContinueAgent(ctx, definition, positional[2], requestID, contractHash, prompt)
			if err != nil {
				return admissionFailure(err)
			}
			if invocationOutput != nil {
				invocationOutput.Operation.Admission = "accepted"
				invocationOutput.Operation.RunID = run.ID
				invocationOutput.Guidance = []cliAction{selectedAction(s, "agent get", "Inspect this admitted run", definition, run.ID)}
			}
			if err := writeJSONData(run); err != nil {
				return err
			}
			if invocationOutput != nil {
				invocationOutput.Status = "accepted"
			}
			return nil
		}
		if action == "start" {
			if len(positional) != 2 {
				return errors.New("agent start takes one definition")
			}
			if requestID == "" {
				requestID, err = newUUID()
				if err != nil {
					return err
				}
			}
			if !deployUUIDRe.MatchString(requestID) {
				return errors.New("--request-id must be a UUID")
			}
			// Print before admission so uncertain responses can be retried with
			// the same request ID rather than creating duplicate work.
			fmt.Fprintf(os.Stderr, "Request ID: %s\n", requestID)
			if invocationOutput != nil {
				invocationOutput.Operation = &cliOperation{Kind: "agent_start", Admission: "preparing", RequestID: requestID, Definition: definition}
				invocationOutput.Guidance = []cliAction{selectedAction(s, "agent list", "Inspect runs without replay; preserve this request ID and exact payload", definition)}
			}
			value := &structpb.Value{}
			if err := value.UnmarshalJSON([]byte(input)); err != nil {
				return err
			}
			if invocationOutput != nil {
				invocationOutput.Operation.Admission = "unknown"
			}
			run, err := client.StartAgent(ctx, definition, requestID, contractHash, value)
			if err != nil {
				return admissionFailure(err)
			}
			if invocationOutput != nil {
				invocationOutput.Operation.Admission = "accepted"
				invocationOutput.Operation.RunID = run.ID
				invocationOutput.Guidance = []cliAction{selectedAction(s, "agent get", "Inspect this admitted run", definition, run.ID)}
				if err := writeJSONData(run); err != nil {
					return err
				}
			}
			if f.wait {
				run, err = client.WaitAgent(ctx, definition, run.ID, 2*time.Second)
				if err != nil {
					return err
				}
			}
			if err := writeJSONData(run); err != nil {
				return err
			}
			if invocationOutput != nil && !f.wait {
				invocationOutput.Status = "accepted"
			}
			if f.wait && run.Status != "completed" {
				return fmt.Errorf("agent run ended with %s", run.Status)
			}
			return nil
		}
		if len(positional) != 3 || !deployUUIDRe.MatchString(positional[2]) {
			return errors.New("agent get|wait|cancel requires definition and run UUID")
		}
		var run *remote.AgentRun
		switch action {
		case "get":
			run, err = client.GetAgent(ctx, definition, positional[2])
		case "wait":
			run, err = client.WaitAgent(ctx, definition, positional[2], 2*time.Second)
		case "cancel":
			run, err = client.CancelAgent(ctx, definition, positional[2])
		default:
			return errors.New("agent action must be start, get, wait or cancel")
		}
		if err != nil {
			return err
		}
		if err := writeJSONData(run); err != nil {
			return err
		}
		if action == "wait" && run.Status != "completed" {
			return fmt.Errorf("agent run ended with %s", run.Status)
		}
		return nil
	case "logs":
		if len(positional) != 1 || !deployUUIDRe.MatchString(positional[0]) {
			return errors.New("logs requires one run UUID")
		}
		response, err := doAuthenticatedHTTP(ctx, s.baseURL, s.token, func(token string) (*http.Request, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL+s.path("/runs/"+url.PathEscape(positional[0])+"/logs"), nil)
			if err == nil {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			return req, err
		})
		if err != nil {
			return err
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(response.Body)
			return newHTTPStatusError(response.StatusCode, response.Status, body)
		}
		return writeResponseBody(response.Body, response.StatusCode, response.Header.Get("Content-Type"), f.json)
	}
	return errors.New("unknown target operation")
}
