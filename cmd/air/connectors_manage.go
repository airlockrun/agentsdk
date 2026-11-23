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
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"
)

func cmdConnectorManagement(args []string) error {
	operation := args[0]
	values := map[string]string{}
	wait, jsonOutput := true, invocationOutput != nil
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--no-wait":
			wait = false
		case "--wait":
			wait = true
		case "--json":
			jsonOutput = true
		case "--url", "--host", "--connector", "--agent", "--need", "--artifact", "--name", "--settings-file", "--timeout", "--request-id":
			flag := args[i]
			if i+1 >= len(args) {
				return fmt.Errorf("%s requires a value", flag)
			}
			if _, exists := values[flag]; exists {
				return fmt.Errorf("duplicate flag %s", flag)
			}
			i++
			values[flag] = args[i]
		default:
			return fmt.Errorf("unknown connectors %s flag %q", operation, args[i])
		}
	}
	requestID := values["--request-id"]
	if requestID == "" {
		requestID = uuid.NewString()
	} else if err := requireManagementUUID(requestID); err != nil {
		return err
	}
	for _, flag := range []string{"--host", "--agent", "--artifact"} {
		if err := requireManagementUUID(values[flag]); err != nil {
			return fmt.Errorf("%s: %w", flag, err)
		}
	}
	if values["--need"] == "" {
		return errors.New("--need is required to select the retained connector artifact")
	}
	if operation == "install" {
		if values["--name"] == "" {
			return errors.New("--name is required for installation")
		}
		if values["--connector"] != "" {
			return errors.New("--connector is only valid for update")
		}
	} else {
		if err := requireManagementUUID(values["--connector"]); err != nil {
			return fmt.Errorf("--connector: %w", err)
		}
		if values["--name"] != "" {
			return errors.New("--name is only valid for installation")
		}
	}
	timeout := 5 * time.Minute
	if value, ok := values["--timeout"]; ok {
		d, err := time.ParseDuration(value)
		if err != nil || d < time.Second || d > 24*time.Hour || d%time.Second != 0 {
			return errors.New("--timeout must be whole seconds between 1s and 24h")
		}
		timeout = d
	}
	settings := ""
	if path, ok := values["--settings-file"]; ok {
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		body, readErr := io.ReadAll(io.LimitReader(file, (1<<20)+1))
		if err := errors.Join(readErr, file.Close()); err != nil {
			return err
		}
		var object map[string]json.RawMessage
		if len(body) > 1<<20 || json.Unmarshal(body, &object) != nil || object == nil {
			return errors.New("settings must be a JSON object no larger than 1 MiB")
		}
		settings = string(body)
	}
	baseURL := values["--url"]
	baseURL, err := connectorOperatorURL(baseURL)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	token, err := accessTokenForURL(ctx, baseURL)
	if err != nil {
		return err
	}
	// Artifact selection is explicit; never pick the latest build implicitly.
	artifacts := &airlockv1.ListConnectorArtifactsResponse{}
	if err := doProto(ctx, baseURL, http.MethodGet, "/api/v1/agents/"+values["--agent"]+"/needs/connector/"+url.PathEscape(values["--need"])+"/artifacts", token, nil, artifacts); err != nil {
		return err
	}
	expectedDigest := ""
	for _, set := range artifacts.ArtifactSets {
		for _, file := range set.Files {
			if file.Id == values["--artifact"] {
				expectedDigest = file.Sha256
			}
		}
	}
	if expectedDigest == "" {
		return errors.New("selected artifact file is not retained for the exact app and connector need")
	}
	if operation == "update" {
		current := &airlockv1.GetConnectorResponse{}
		if err := doProto(ctx, baseURL, http.MethodGet, "/api/v1/connectors/"+values["--connector"], token, nil, current); err != nil {
			return err
		}
		if current.Connector == nil || current.Connector.Id != values["--connector"] || current.Connector.HostId != values["--host"] {
			return errors.New("connector does not belong to the exact requested host")
		}
	}
	var request proto.Message
	path := "/api/v1/hosts/" + values["--host"] + "/connectors"
	if operation == "install" {
		request = &airlockv1.RequestConnectorInstallRequest{RequestId: requestID, AgentId: values["--agent"], NeedSlug: values["--need"], ArtifactFileId: values["--artifact"], DisplayName: values["--name"], SettingsJson: settings, TimeoutSeconds: int32(timeout / time.Second)}
	} else {
		path = "/api/v1/connectors/" + values["--connector"] + "/update"
		request = &airlockv1.RequestConnectorUpdateRequest{RequestId: requestID, ArtifactFileId: values["--artifact"], SettingsJson: settings, TimeoutSeconds: int32(timeout / time.Second)}
	}
	response := &airlockv1.GetHostManagementJobResponse{}
	fmt.Fprintf(os.Stderr, "Remote admission request %s; preserve this ID and exact arguments after an uncertain response.\n", requestID)
	if invocationOutput != nil {
		invocationOutput.Operation = &cliOperation{Kind: "connector_" + operation, Admission: "unknown", RequestID: requestID, HostID: values["--host"], ConnectorID: values["--connector"]}
		invocationOutput.Guidance = []cliAction{{Kind: "observe", Message: "Recover exact admission without resubmitting", Executable: "go", Args: []string{"tool", "air", "hosts", "request", values["--host"], requestID, "--url", baseURL, "--json"}}}
	}
	if err := doProto(ctx, baseURL, http.MethodPost, path, token, request, response); err != nil {
		return admissionFailure(err)
	}
	if err := validateConnectorManagementJob(response.Job, values["--host"], values["--connector"], "connector_"+operation, values["--artifact"]); err != nil {
		return err
	}
	if response.Job.RequestId != requestID {
		return errors.New("Airlock returned a job for a different admission request")
	}
	jobID, connectorID := response.Job.Id, response.Job.ConnectorId
	if invocationOutput != nil {
		invocationOutput.Operation.Admission = "accepted"
		invocationOutput.Operation.JobID = jobID
		invocationOutput.Operation.ConnectorID = connectorID
	}
	if invocationOutput != nil {
		if err := writeProtoJSON(response); err != nil {
			return err
		}
	}
	if !wait {
		if jsonOutput {
			if err := writeProtoJSON(response); err != nil {
				return err
			}
			if invocationOutput != nil {
				invocationOutput.Status = "accepted"
			}
			return nil
		}
		fmt.Printf("Connector %s accepted — not yet verified.\n", operation)
		fmt.Printf("Observe: %s\n", shellCommand(cliAction{Executable: "go", Args: []string{"tool", "air", "hosts", "job", jobID, "--url", baseURL, "--wait"}}))
		return nil
	}
	fmt.Fprintf(os.Stderr, "Remote job %s on host %s; interrupt stops observation, not execution.\n", jobID, values["--host"])
	// The server deadline bounds execution. Allow bounded delivery settlement;
	// expiry never causes resubmission of an operation with unknown effects.
	observe, cancel := context.WithTimeout(ctx, timeout+30*time.Second)
	defer cancel()
	lastStatus := ""
	var lastEvent int64
	for {
		if invocationOutput != nil {
			if err := writeProtoJSON(response); err != nil {
				return err
			}
		}
		for _, event := range response.Events {
			if event.Sequence > lastEvent {
				fmt.Fprintf(os.Stderr, "%s: %s %q\n", jobID, event.Phase, event.Message)
				lastEvent = event.Sequence
			}
		}
		if response.Job.Status != lastStatus {
			fmt.Fprintf(os.Stderr, "%s: %s\n", jobID, response.Job.Status)
			lastStatus = response.Job.Status
		}
		switch response.Job.Status {
		case "failed", "cancelled", "timed_out":
			if jsonOutput {
				if err := writeProtoJSON(response); err != nil {
					return err
				}
			}
			return fmt.Errorf("remote job %s %s: %s", jobID, response.Job.Status, response.Job.ErrorMessage)
		case "succeeded":
			if response.Job.InventorySupersededAt != nil {
				if jsonOutput {
					if err := writeProtoJSON(response); err != nil {
						return err
					}
				}
				return fmt.Errorf("job %s completed but its inventory was superseded before verification; inspect the current connector, do not claim this request verified", jobID)
			}
			current := &airlockv1.GetConnectorResponse{}
			if err := doProto(observe, baseURL, http.MethodGet, "/api/v1/connectors/"+connectorID, token, nil, current); err != nil {
				return fmt.Errorf("job %s succeeded, inventory verification failed: %w", jobID, err)
			}
			c := current.Connector
			if c == nil || c.Id != connectorID || c.HostId != values["--host"] {
				return errors.New("Airlock returned connector inventory for a different target")
			}
			if response.Job.InventoryRevision != nil && response.Job.GetInventoryRevision() > 0 && response.Job.InventoryAcknowledgedAt != nil && !response.Job.VerificationPending && c.InventoryRevision >= uint64(response.Job.GetInventoryRevision()) && c.Readiness == "ready" && c.ArtifactDigest == expectedDigest && c.ActiveObservationState == "known" && c.ObservedActiveDigest == expectedDigest {
				if jsonOutput {
					return writeProtoJSON(response)
				}
				name := c.DisplayName
				if name == "" {
					name = "Connector"
				}
				fmt.Printf("%s ready — verified %s.\n", safeTableCell(name), operation)
				return nil
			}
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-observe.Done():
			timer.Stop()
			return fmt.Errorf("observation stopped; job %s status %s, installation/update not verified; do not resubmit without inspection: %w", jobID, response.Job.Status, observe.Err())
		case <-timer.C:
		}
		response = &airlockv1.GetHostManagementJobResponse{}
		if err := doProto(observe, baseURL, http.MethodGet, "/api/v1/host-management-jobs/"+jobID, token, nil, response); err != nil {
			return fmt.Errorf("observe job %s (no resubmission): %w", jobID, err)
		}
		if response.Job == nil || response.Job.Id != jobID {
			return errors.New("Airlock returned a different management job")
		}
		if response.Job.RequestId != requestID {
			return errors.New("Airlock returned a job for a different admission request")
		}
		if err := validateConnectorManagementJob(response.Job, values["--host"], connectorID, "connector_"+operation, values["--artifact"]); err != nil {
			return err
		}
	}
}

func requireManagementUUID(value string) error {
	id, err := uuid.Parse(value)
	if err != nil || id == uuid.Nil || id.String() != value {
		return errors.New("requires an exact canonical nonzero UUID")
	}
	return nil
}

func validateConnectorManagementJob(job *airlockv1.HostManagementJobInfo, host, connector, kind, artifact string) error {
	if job == nil || requireManagementUUID(job.Id) != nil || requireManagementUUID(job.ConnectorId) != nil || job.HostId != host || job.Kind != kind || job.ArtifactFileId != artifact || connector != "" && job.ConnectorId != connector {
		return errors.New("Airlock returned a management job for a different target")
	}
	switch strings.TrimSpace(job.Status) {
	case "queued", "running", "succeeded", "failed", "cancelled", "timed_out":
		return nil
	default:
		return errors.New("Airlock returned an unknown management job status")
	}
}
