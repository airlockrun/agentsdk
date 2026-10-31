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
	fmt.Fprintf(os.Stderr, "Target: remote=%s app=%s target=%s runtime=%s revision=%s\n", s.remoteName, s.app.AgentID, s.info.Name, s.info.Id, s.info.SourceRevisionId)
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
		if _, err := io.Copy(os.Stdout, response.Body); err != nil {
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
		out, err := client.InvokeTool(ctx, positional[1], value)
		if err != nil {
			return err
		}
		return writeProtoJSON(out)
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
			return json.NewEncoder(os.Stdout).Encode(runs)
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
			run, err := client.ContinueAgent(ctx, definition, positional[2], requestID, contractHash, prompt)
			if err != nil {
				return err
			}
			return json.NewEncoder(os.Stdout).Encode(run)
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
			value := &structpb.Value{}
			if err := value.UnmarshalJSON([]byte(input)); err != nil {
				return err
			}
			run, err := client.StartAgent(ctx, definition, requestID, contractHash, value)
			if err != nil {
				return err
			}
			if f.wait {
				run, err = client.WaitAgent(ctx, definition, run.ID, 2*time.Second)
				if err != nil {
					return err
				}
			}
			if err := json.NewEncoder(os.Stdout).Encode(run); err != nil {
				return err
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
		if err := json.NewEncoder(os.Stdout).Encode(run); err != nil {
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
		_, err = io.Copy(os.Stdout, response.Body)
		return err
	}
	return errors.New("unknown target operation")
}
