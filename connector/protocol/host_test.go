package protocol

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
)

func TestChildFramesRoundTripAndRejectUnknownFields(t *testing.T) {
	var stream bytes.Buffer
	input := ChildInitialize{ProtocolVersion: HostProtocolVersion, InstallationID: "one", StateDirectory: "/state", Settings: []byte(`{}`)}
	if err := NewChildEncoder(&stream).Encode(ChildEnvelope{Type: ChildMessageInitialize, Initialize: &input}); err != nil {
		t.Fatal(err)
	}
	var decoded ChildEnvelope
	if err := NewChildDecoder(&stream).Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Initialize == nil || decoded.Initialize.InstallationID != "one" {
		t.Fatalf("decoded = %+v", decoded)
	}

	body := []byte(`{"type":"cancel","cancel":{"jobId":"job","attemptToken":"attempt","extra":true}}`)
	stream.Reset()
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(body)))
	stream.Write(header[:])
	stream.Write(body)
	if err := NewChildDecoder(&stream).Decode(&decoded); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("error = %v", err)
	}
}

func TestHostedManifestSummaryKeepsMaximumInventoryBounded(t *testing.T) {
	features := make([]string, 64)
	for i := range features {
		features[i] = fmt.Sprintf("feature-%02d-%s", i, strings.Repeat("a", 52))
	}
	manifest := HostedConnectorManifest{ProtocolMajor: Major, ProtocolMinor: Minor, Features: features, ArtifactVersion: strings.Repeat("\x01", 256), ArtifactDigest: strings.Repeat("a", 64), InterfaceHash: strings.Repeat("b", 64)}
	if err := ValidateHostedConnectorManifest(manifest); err != nil {
		t.Fatal(err)
	}
	request := validHostSyncRequest()
	request.Connectors = make([]HostedConnectorStatus, MaxHostedConnectors)
	for i := range request.Connectors {
		request.Connectors[i] = HostedConnectorStatus{InstallationID: fmt.Sprintf("%08x-0000-4000-8000-%012x", i+1, i+1), Manifest: manifest, Readiness: ReadinessUnhealthy, Error: strings.Repeat("\x01", MaxHostedStatusErrorBytes)}
	}
	if err := ValidateHostSyncRequest(request); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > MaxHostSyncBytes {
		t.Fatalf("maximum compact inventory is %d bytes", len(body))
	}
}

func TestValidateHostedConnectorManifestProtocolMinorBound(t *testing.T) {
	manifest := SummarizeManifest(validProtocolTestManifest(t))
	manifest.ProtocolMinor = math.MaxInt32
	if err := ValidateHostedConnectorManifest(manifest); err != nil {
		t.Fatal(err)
	}
	manifest.ProtocolMinor++
	if err := ValidateHostedConnectorManifest(manifest); err == nil {
		t.Fatal("hosted manifest accepted protocol minor above math.MaxInt32")
	}
}

func TestValidateHostSyncRequestBounds(t *testing.T) {
	manifest := SummarizeManifest(validProtocolTestManifest(t))
	status := HostedConnectorStatus{InstallationID: "11111111-1111-4111-8111-111111111111", Manifest: manifest, Readiness: ReadinessReady}

	tests := []struct {
		name   string
		mutate func(*HostSyncRequest)
	}{
		{name: "connector count", mutate: func(request *HostSyncRequest) {
			request.Connectors = make([]HostedConnectorStatus, MaxHostedConnectors+1)
		}},
		{name: "duplicate installation", mutate: func(request *HostSyncRequest) {
			request.Connectors = append(request.Connectors, request.Connectors[0])
		}},
		{name: "readiness enum", mutate: func(request *HostSyncRequest) {
			request.Connectors[0].Readiness = "broken"
		}},
		{name: "active attempt count", mutate: func(request *HostSyncRequest) {
			request.Connectors[0].ActiveAttempts = make([]ActiveAttempt, MaxActiveAttempts+1)
		}},
		{name: "status error", mutate: func(request *HostSyncRequest) {
			request.Connectors[0].Error = strings.Repeat("x", MaxHostedStatusErrorBytes+1)
		}},
		{name: "aggregate bytes", mutate: func(request *HostSyncRequest) {
			request.Connectors[0].ActiveAttempts = []ActiveAttempt{{JobID: strings.Repeat("x", MaxHostSyncBytes)}}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := validHostSyncRequest()
			request.Connectors = []HostedConnectorStatus{status}
			test.mutate(&request)
			if err := ValidateHostSyncRequest(request); err == nil {
				t.Fatal("invalid host sync accepted")
			}
		})
	}
}

func TestValidateHostConnectorInventoryMutationRequest(t *testing.T) {
	valid := validInventoryMutation(t)
	if err := ValidateHostConnectorInventoryMutationRequest(valid); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		mutate func(*HostConnectorInventoryMutationRequest)
	}{
		{name: "noncanonical installation UUID", mutate: func(request *HostConnectorInventoryMutationRequest) {
			request.InstallationID = "AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA"
		}},
		{name: "zero installation UUID", mutate: func(request *HostConnectorInventoryMutationRequest) {
			request.InstallationID = "00000000-0000-0000-0000-000000000000"
		}},
		{name: "zero revision", mutate: func(request *HostConnectorInventoryMutationRequest) {
			request.Revision = 0
		}},
		{name: "mutation enum", mutate: func(request *HostConnectorInventoryMutationRequest) {
			request.Kind = "replace"
		}},
		{name: "missing display name", mutate: func(request *HostConnectorInventoryMutationRequest) {
			request.DisplayName = " "
		}},
		{name: "display name bound", mutate: func(request *HostConnectorInventoryMutationRequest) {
			request.DisplayName = strings.Repeat("x", MaxHostDisplayNameBytes+1)
		}},
		{name: "missing active artifact", mutate: func(request *HostConnectorInventoryMutationRequest) {
			request.Active = nil
		}},
		{name: "malformed active manifest", mutate: func(request *HostConnectorInventoryMutationRequest) {
			request.Active.Manifest.ProtocolMajor++
		}},
		{name: "malformed measured digest", mutate: func(request *HostConnectorInventoryMutationRequest) {
			request.Active.MeasuredDigest = strings.Repeat("A", 64)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := validInventoryMutation(t)
			test.mutate(&request)
			if err := ValidateHostConnectorInventoryMutationRequest(request); err == nil {
				t.Fatal("invalid inventory mutation accepted")
			}
		})
	}
}

func TestInventoryMutationRevisionBounds(t *testing.T) {
	request := validInventoryMutation(t)
	request.Revision = math.MaxInt64
	if err := ValidateHostConnectorInventoryMutationRequest(request); err != nil {
		t.Fatal(err)
	}
	request.Revision++
	if err := ValidateHostConnectorInventoryMutationRequest(request); err == nil {
		t.Fatal("inventory mutation accepted revision above math.MaxInt64")
	}

	response := HostConnectorInventoryMutationResponse{InstallationID: request.InstallationID, AcknowledgedRevision: math.MaxInt64}
	if err := ValidateHostConnectorInventoryMutationResponse(response); err != nil {
		t.Fatal(err)
	}
	response.AcknowledgedRevision++
	if err := ValidateHostConnectorInventoryMutationResponse(response); err == nil {
		t.Fatal("inventory acknowledgement accepted revision above math.MaxInt64")
	}
}

func TestInventoryMutationRejectsDigestManifestDisagreement(t *testing.T) {
	for _, slot := range []string{"active", "rollback"} {
		t.Run(slot, func(t *testing.T) {
			request := validInventoryMutation(t)
			rollback := *request.Active
			request.Rollback = &rollback
			if slot == "active" {
				request.Active.MeasuredDigest = strings.Repeat("b", 64)
			} else {
				request.Rollback.MeasuredDigest = strings.Repeat("b", 64)
			}
			if err := ValidateHostConnectorInventoryMutationRequest(request); err == nil || !strings.Contains(err.Error(), "does not match") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestInventoryMutationTombstone(t *testing.T) {
	request := HostConnectorInventoryMutationRequest{
		InstallationID: "11111111-1111-4111-8111-111111111111",
		Revision:       8,
		Kind:           HostConnectorMutationRemove,
	}
	if err := ValidateHostConnectorInventoryMutationRequest(request); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"displayName", "active", "rollback"} {
		if bytes.Contains(body, []byte(field)) {
			t.Fatalf("tombstone contains %q: %s", field, body)
		}
	}

	invalid := request
	invalid.DisplayName = "removed"
	if err := ValidateHostConnectorInventoryMutationRequest(invalid); err == nil {
		t.Fatal("tombstone with upsert fields accepted")
	}
}

func TestInventoryMutationCarriesRollbackManifest(t *testing.T) {
	request := validInventoryMutation(t)
	rollbackManifest := validProtocolTestManifest(t)
	rollbackManifest.ArtifactDigest = strings.Repeat("b", 64)
	request.Rollback = &ObservedConnectorArtifact{Manifest: rollbackManifest, MeasuredDigest: rollbackManifest.ArtifactDigest}
	if err := ValidateHostConnectorInventoryMutationRequest(request); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte(`"rollback":{"manifest":`)) || bytes.Contains(body, []byte(`"settingsValues"`)) {
		t.Fatalf("unexpected mutation JSON: %s", body)
	}
}

func TestValidateHostConnectorInventoryMutationResponse(t *testing.T) {
	valid := HostConnectorInventoryMutationResponse{
		InstallationID:       "11111111-1111-4111-8111-111111111111",
		AcknowledgedRevision: 9,
		StorageOrigins:       []string{"https://artifacts.example.com", "https://storage.example.com:8443"},
	}
	if err := ValidateHostConnectorInventoryMutationResponse(valid); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		mutate func(*HostConnectorInventoryMutationResponse)
	}{
		{name: "installation UUID", mutate: func(response *HostConnectorInventoryMutationResponse) {
			response.InstallationID = "not-a-uuid"
		}},
		{name: "zero acknowledgement", mutate: func(response *HostConnectorInventoryMutationResponse) {
			response.AcknowledgedRevision = 0
		}},
		{name: "origin count", mutate: func(response *HostConnectorInventoryMutationResponse) {
			response.StorageOrigins = make([]string, MaxHostStorageOrigins+1)
		}},
		{name: "insecure origin", mutate: func(response *HostConnectorInventoryMutationResponse) {
			response.StorageOrigins = []string{"http://storage.example.com"}
		}},
		{name: "origin path", mutate: func(response *HostConnectorInventoryMutationResponse) {
			response.StorageOrigins = []string{"https://storage.example.com/path"}
		}},
		{name: "noncanonical origin", mutate: func(response *HostConnectorInventoryMutationResponse) {
			response.StorageOrigins = []string{"https://STORAGE.example.com"}
		}},
		{name: "duplicate origin", mutate: func(response *HostConnectorInventoryMutationResponse) {
			response.StorageOrigins = []string{"https://storage.example.com", "https://storage.example.com"}
		}},
		{name: "origin length", mutate: func(response *HostConnectorInventoryMutationResponse) {
			response.StorageOrigins = []string{"https://" + strings.Repeat("a", MaxHostStorageOriginBytes) + ".example.com"}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := valid
			test.mutate(&response)
			if err := ValidateHostConnectorInventoryMutationResponse(response); err == nil {
				t.Fatal("invalid inventory acknowledgement accepted")
			}
		})
	}
}

func TestChildEnvelopeRequiresExactlyMatchingPayload(t *testing.T) {
	cancel := ChildCancel{JobID: "job", AttemptToken: "attempt"}
	if err := ValidateChildEnvelope(ChildEnvelope{Type: ChildMessageJob, Cancel: &cancel}); err == nil {
		t.Fatal("mismatched payload accepted")
	}
	if err := ValidateChildEnvelope(ChildEnvelope{Type: ChildMessageCancel, Cancel: &cancel, Job: &JobRequest{}}); err == nil {
		t.Fatal("multiple payloads accepted")
	}
}

func TestChildDecoderRejectsOversizedFrameBeforeAllocation(t *testing.T) {
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], MaxChildFrameBytes+1)
	var envelope ChildEnvelope
	if err := NewChildDecoder(bytes.NewReader(header[:])).Decode(&envelope); err == nil {
		t.Fatal("oversized frame accepted")
	}
}

func validHostSyncRequest() HostSyncRequest {
	return HostSyncRequest{Host: HostInfo{
		ProtocolVersion: HostProtocolVersion,
		Name:            "test-host",
		Platform:        "linux",
		Architecture:    "amd64",
		AccessMode:      RemoteAccessFull,
		Version:         "1.0.0",
	}}
}

func validInventoryMutation(t *testing.T) HostConnectorInventoryMutationRequest {
	t.Helper()
	manifest := validProtocolTestManifest(t)
	return HostConnectorInventoryMutationRequest{
		InstallationID: "11111111-1111-4111-8111-111111111111",
		Revision:       7,
		Kind:           HostConnectorMutationUpsert,
		DisplayName:    "Test connector",
		Active:         &ObservedConnectorArtifact{Manifest: manifest, MeasuredDigest: manifest.ArtifactDigest},
	}
}
