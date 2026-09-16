package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestHostMessageStrictEnvelope(t *testing.T) {
	for _, test := range []struct {
		name, data string
		valid      bool
	}{
		{"demand", `{"protocol":"airlock.host.v2","id":"1","demand":{"connectorCapacity":1}}`, true},
		{"missing version", `{"id":"1","ack":{}}`, false},
		{"wrong version", `{"protocol":"airlock.host.v1","id":"1","ack":{}}`, false},
		{"missing correlation", `{"protocol":"airlock.host.v2","ack":{}}`, false},
		{"unknown field", `{"protocol":"airlock.host.v2","id":"1","ack":{},"extra":true}`, false},
		{"unknown nested field", `{"protocol":"airlock.host.v2","id":"1","demand":{"connectorCapacity":1,"extra":true}}`, false},
		{"two payloads", `{"protocol":"airlock.host.v2","id":"1","ack":{},"demand":{"connectorCapacity":1}}`, false},
		{"negative capacity", `{"protocol":"airlock.host.v2","id":"1","demand":{"connectorCapacity":-1}}`, false},
		{"trailing value", `{"protocol":"airlock.host.v2","id":"1","ack":{}}{}`, false},
		{"oversized", strings.Repeat(" ", MaxHostMessageBytes+1), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := DecodeHostMessage([]byte(test.data))
			if (err == nil) != test.valid {
				t.Fatalf("DecodeHostMessage = %v", err)
			}
		})
	}
}

func TestHostCompletionEnvelopeRoundTrip(t *testing.T) {
	message := HostMessage{Protocol: HostTransportProtocol, ID: "completion", ConnectorCompletion: &HostConnectorCompletion{ConnectorID: "connector", JobID: "job", Completion: JobCompletion{AttemptToken: "attempt", Status: "success", Output: json.RawMessage(`{"ok":true}`)}}}
	body, err := json.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeHostMessage(body)
	if err != nil || decoded.ConnectorCompletion == nil || string(decoded.ConnectorCompletion.Completion.Output) != `{"ok":true}` {
		t.Fatalf("round trip = %+v, %v", decoded, err)
	}
}
