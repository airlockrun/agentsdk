package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/airlockrun/agentsdk/connector/protocol"
	airlockv1 "github.com/airlockrun/agentsdk/internal/airlockv1"
	"github.com/google/uuid"
)

type hostCommandExitError struct{ code int }

func (e *hostCommandExitError) Error() string {
	return fmt.Sprintf("remote command exited with status %d", e.code)
}

// cmdHosts uses ordinary CLI authentication. Developer tokens retain their
// server-enforced execution ceiling; command flags never establish authority.
func cmdHosts(args []string) error {
	if len(args) < 2 || (args[0] != "exec" && args[0] != "job" && args[0] != "cancel" && args[0] != "request") {
		return errors.New("hosts requires: exec <host-UUID> [--url URL] [--cwd DIR] [--env KEY=VALUE] [--stdin-file PATH] [--timeout DURATION] [--no-wait] [--json] -- COMMAND [ARG...] or job|cancel <job-UUID> [--url URL] [--wait] [--json]")
	}
	id, err := uuid.Parse(args[1])
	if err != nil || id == uuid.Nil || id.String() != args[1] {
		return errors.New("hosts requires an exact canonical nonzero UUID")
	}
	request := &airlockv1.RequestHostShellRequest{RequestId: uuid.NewString(), TimeoutSeconds: 300, Environment: map[string]string{}}
	start := 2
	lookupID := ""
	if args[0] == "request" {
		if len(args) < 3 {
			return errors.New("hosts request requires HOST_UUID REQUEST_UUID")
		}
		if err := requireManagementUUID(args[2]); err != nil {
			return err
		}
		lookupID = args[2]
		start = 3
	}
	baseURL, stdinFile := "", ""
	wait, jsonOutput := args[0] == "exec", invocationOutput != nil
	for i := start; i < len(args); i++ {
		flag := args[i]
		if flag == "--" && args[0] == "exec" {
			if i+1 >= len(args) {
				return errors.New("-- requires a command")
			}
			request.Command, request.Arguments = args[i+1], args[i+2:]
			break
		}
		switch flag {
		case "--wait":
			wait = true
		case "--no-wait":
			wait = false
		case "--json":
			jsonOutput = true
		case "--url", "--cwd", "--env", "--stdin-file", "--timeout", "--request-id":
			if i+1 >= len(args) {
				return fmt.Errorf("%s requires a value", flag)
			}
			if args[0] != "exec" && flag != "--url" {
				return fmt.Errorf("%s is only valid for hosts exec", flag)
			}
			i++
			value := args[i]
			switch flag {
			case "--request-id":
				if err := requireManagementUUID(value); err != nil {
					return err
				}
				request.RequestId = value
			case "--url":
				baseURL = normalizeBaseURL(value)
			case "--cwd":
				request.WorkingDirectory = value
			case "--stdin-file":
				stdinFile = value
			case "--env":
				key, value, ok := strings.Cut(value, "=")
				if !ok || key == "" || strings.ContainsRune(key+value, '\x00') {
					return errors.New("--env requires KEY=VALUE without NUL")
				}
				if _, exists := request.Environment[key]; exists {
					return fmt.Errorf("duplicate environment key %q", key)
				}
				request.Environment[key] = value
			case "--timeout":
				d, err := time.ParseDuration(value)
				if err != nil || d < time.Second || d > 24*time.Hour || d%time.Second != 0 {
					return errors.New("--timeout must be whole seconds between 1s and 24h")
				}
				request.TimeoutSeconds = int32(d / time.Second)
			}
		default:
			return fmt.Errorf("unknown hosts flag %q; put command arguments after --", flag)
		}
	}
	if args[0] == "exec" && strings.TrimSpace(request.Command) == "" {
		return errors.New("hosts exec requires -- COMMAND [ARG...]")
	}
	if stdinFile != "" {
		file, err := os.Open(stdinFile)
		if err != nil {
			return err
		}
		request.Stdin, err = io.ReadAll(io.LimitReader(file, (1<<20)+1))
		closeErr := file.Close()
		if err != nil || closeErr != nil {
			return errors.Join(err, closeErr)
		}
		if len(request.Stdin) > 1<<20 {
			return errors.New("stdin file exceeds 1 MiB")
		}
	}
	if baseURL == "" && os.Getenv("AIRLOCK_DEVELOPMENT_SESSION") != "" {
		baseURL = os.Getenv("AIRLOCK_API_URL")
	}
	baseURL, err = connectorOperatorURL(baseURL)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	token, err := accessTokenForURL(ctx, baseURL)
	if err != nil {
		return err
	}
	response := &airlockv1.GetHostManagementJobResponse{}
	if args[0] == "exec" {
		if invocationOutput != nil {
			invocationOutput.Operation = &cliOperation{Kind: "host_shell", Admission: "unknown", RequestID: request.RequestId, HostID: id.String()}
			invocationOutput.Guidance = []cliAction{{Kind: "observe", Message: "Recover this exact request; do not admit a new request after uncertainty", Executable: "go", Args: []string{"tool", "air", "hosts", "request", id.String(), request.RequestId, "--url", baseURL, "--json"}}}
		}
		fmt.Fprintf(os.Stderr, "Remote admission request %s; preserve this ID and exact arguments after an uncertain response.\n", request.RequestId)
		err = doProto(ctx, baseURL, http.MethodPost, "/api/v1/hosts/"+id.String()+"/shell", token, request, response)
		if err != nil {
			err = admissionFailure(err)
		}
	} else {
		method := http.MethodGet
		if args[0] == "cancel" {
			method = http.MethodDelete
		}
		path := "/api/v1/host-management-jobs/" + id.String()
		if args[0] == "request" {
			path = "/api/v1/hosts/" + id.String() + "/management-requests/" + lookupID
		}
		err = doProto(ctx, baseURL, method, path, token, nil, response)
	}
	if err != nil {
		var status *httpStatusError
		if args[0] == "request" && errors.As(err, &status) && status.StatusCode == http.StatusNotFound {
			message := "Request lookup is unavailable on this instance or the request was not found; neither proves the original operation was not admitted. Preserve the exact request ID and payload; inspect a recorded job ID with hosts job. Do not submit a fresh request."
			if invocationOutput != nil {
				invocationOutput.Guidance = []cliAction{{Kind: "recovery", Message: message}}
			}
			return fmt.Errorf("recover host %s request %s: %s: %w", id.String(), lookupID, message, err)
		}
		return err
	}
	identityCommand := args[0]
	if identityCommand == "cancel" {
		identityCommand = "job"
	}
	if err := validateHostJob(response.Job, identityCommand, id.String()); err != nil {
		return err
	}
	if args[0] == "exec" && response.Job.RequestId != request.RequestId {
		return errors.New("Airlock returned a job for a different admission request")
	}
	if args[0] == "request" && response.Job.RequestId != lookupID {
		return errors.New("Airlock returned a job for a different admission request")
	}
	if args[0] == "cancel" && response.Job.CancelRequestedAt == nil && (response.Job.Status == "queued" || response.Job.Status == "running") {
		return errors.New("Airlock did not confirm the cancellation request")
	}
	jobID, hostID := response.Job.Id, response.Job.HostId
	if invocationOutput != nil && args[0] == "exec" {
		invocationOutput.Operation.Admission = "accepted"
		invocationOutput.Operation.JobID = jobID
	}
	if invocationOutput != nil {
		if err := writeProtoJSON(response); err != nil {
			return err
		}
	}
	kind := response.Job.Kind
	if wait {
		bound := time.Duration(request.TimeoutSeconds)*time.Second + 30*time.Second
		if response.Job.DeadlineAt != nil {
			if err := response.Job.DeadlineAt.CheckValid(); err != nil {
				return errors.New("Airlock returned an invalid job deadline")
			}
			bound = time.Until(response.Job.DeadlineAt.AsTime().Add(30 * time.Second))
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, bound)
		defer cancel()
		fmt.Fprintf(os.Stderr, "Remote job %s on host %s; interrupt stops observation, not execution.\n", jobID, hostID)
		for response.Job.Status == "queued" || response.Job.Status == "running" {
			timer := time.NewTimer(time.Second)
			select {
			case <-ctx.Done():
				timer.Stop()
				return fmt.Errorf("observation interrupted; job %s may still run until its deadline: %w", jobID, ctx.Err())
			case <-timer.C:
			}
			response = &airlockv1.GetHostManagementJobResponse{}
			if err := doProto(ctx, baseURL, http.MethodGet, "/api/v1/host-management-jobs/"+jobID, token, nil, response); err != nil {
				return fmt.Errorf("observe job %s (no resubmission): %w", jobID, err)
			}
			if err := validateHostJob(response.Job, "job", jobID); err != nil {
				return err
			}
			if response.Job.HostId != hostID || response.Job.Kind != kind {
				return errors.New("Airlock returned a job for a different host")
			}
			if invocationOutput != nil {
				if err := writeProtoJSON(response); err != nil {
					return err
				}
			}
		}
	}
	if jsonOutput {
		if err := writeProtoJSON(response); err != nil {
			return err
		}
		if !wait {
			if invocationOutput != nil && (args[0] == "exec" || args[0] == "cancel") {
				invocationOutput.Status = "accepted"
			}
			return nil
		}
	}
	if args[0] == "cancel" && wait && response.Job.Status == "cancelled" {
		if !jsonOutput {
			fmt.Printf("Job %s cancellation settled; earlier effects may exist.\n", jobID)
		}
		return nil
	}
	if args[0] == "cancel" && wait {
		if !jsonOutput {
			fmt.Printf("Job %s is %s; cancellation does not undo completed effects.\n", jobID, response.Job.Status)
		}
		return nil
	}
	if !wait {
		if args[0] == "cancel" && !jsonOutput {
			if response.Job.CancelRequestedAt == nil {
				fmt.Printf("Job %s is already %s; no cancellation was requested. Earlier effects may exist.\n", jobID, response.Job.Status)
				return nil
			}
			fmt.Printf("Cancellation requested for job %s on host %s; status %s. Earlier effects may exist.\n", jobID, hostID, response.Job.Status)
			return nil
		}
		status := response.Job.Status
		if response.Job.InventorySupersededAt != nil {
			status += " (inventory superseded; not verified)"
		} else if response.Job.VerificationPending {
			status += " (inventory verification pending)"
		}
		fmt.Printf("%s\t%s\t%s\n", jobID, hostID, status)
		return nil
	}
	if response.Job.Kind != "shell" {
		if response.Job.Status != "succeeded" {
			return fmt.Errorf("remote job %s %s: %s", jobID, response.Job.Status, response.Job.ErrorMessage)
		}
		if !jsonOutput {
			fmt.Printf("Job %s succeeded; inspect connector inventory to verify the active installation.\n", jobID)
		}
		return nil
	}
	var output protocol.ShellOutput
	if response.Job.OutputJson != "" {
		var err error
		output, err = decodeShellOutput(jobID, response.Job.OutputJson)
		if err != nil {
			return err
		}
		if !jsonOutput {
			if _, err := os.Stdout.Write(output.Stdout); err != nil {
				return err
			}
			if _, err := os.Stderr.Write(output.Stderr); err != nil {
				return err
			}
		}
	}
	if response.Job.Status != "succeeded" {
		return fmt.Errorf("remote job %s %s: %s", jobID, response.Job.Status, response.Job.ErrorMessage)
	}
	if response.Job.OutputJson == "" {
		return errors.New("Airlock returned a successful shell job without output")
	}
	if output.ExitCode != 0 {
		if output.ExitCode < 1 || output.ExitCode > 255 {
			return fmt.Errorf("remote command terminated with exit status %s", strconv.Itoa(output.ExitCode))
		}
		return &hostCommandExitError{code: output.ExitCode}
	}
	return nil
}

func decodeShellOutput(jobID, raw string) (protocol.ShellOutput, error) {
	var output protocol.ShellOutput
	if err := json.Unmarshal([]byte(raw), &output); err != nil {
		return output, fmt.Errorf("invalid shell output for job %s: %w", jobID, err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil || len(fields["exitCode"]) == 0 || string(fields["exitCode"]) == "null" {
		return output, fmt.Errorf("shell output for job %s has no exit status", jobID)
	}
	return output, nil
}

func writeHostObservation(response *airlockv1.GetHostManagementJobResponse) error {
	raw, err := protoMarshal.Marshal(response)
	if err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	for _, key := range []string{"stdout", "stderr", "exitCode"} {
		fields[key] = json.RawMessage("null")
	}
	// Retain the typed snapshot even when a malformed child output cannot be decoded.
	setResult(fields, "completed")
	job := response.Job
	if job != nil && job.Kind == "shell" && job.OutputJson != "" {
		output, err := decodeShellOutput(job.Id, job.OutputJson)
		if err != nil {
			return err
		}
		fields["stdout"], err = json.Marshal(encodedBody(output.Stdout, 0, ""))
		if err != nil {
			return err
		}
		fields["stderr"], err = json.Marshal(encodedBody(output.Stderr, 0, ""))
		if err != nil {
			return err
		}
		fields["exitCode"], err = json.Marshal(output.ExitCode)
		if err != nil {
			return err
		}
	}
	return writeJSONData(fields)
}

func validateHostJob(job *airlockv1.HostManagementJobInfo, command, id string) error {
	if job == nil {
		return errors.New("Airlock returned no management job")
	}
	if command == "exec" && job.Kind != "shell" {
		return errors.New("Airlock returned no shell job")
	}
	switch job.Kind {
	case "shell", "connector_install", "connector_update", "connector_rollback", "connector_remove":
	default:
		return errors.New("Airlock returned an unknown management job kind")
	}
	for _, value := range []string{job.Id, job.HostId} {
		parsed, err := uuid.Parse(value)
		if err != nil || parsed == uuid.Nil || parsed.String() != value {
			return errors.New("Airlock returned an invalid shell job identity")
		}
	}
	if (command == "exec" || command == "request") && job.HostId != id || command == "job" && job.Id != id {
		return errors.New("Airlock returned a shell job for a different target")
	}
	switch job.Status {
	case "queued", "running", "succeeded", "failed", "cancelled", "timed_out":
		return nil
	default:
		return errors.New("Airlock returned an unknown shell job status")
	}
}
