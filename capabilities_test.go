package agentsdk

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/airlockrun/agentsdk/wire"
)

func TestCapabilityManifest(t *testing.T) {
	for _, tc := range []struct {
		name     string
		features Capabilities
	}{
		{"default", Capabilities{}},
		{"devices", Capabilities{Microphone: true, Camera: true}},
		{"all", Capabilities{Microphone: true, Camera: true, Geolocation: true, DisplayCapture: true, Fullscreen: true, ClipboardWrite: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{Description: "Device app", Capabilities: tc.features}
			a := New(cfg)
			cfg.Capabilities = Capabilities{}
			first := a.Manifest()
			raw, err := json.Marshal(first)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(raw, []byte(`"capabilities"`)) != (tc.features != (Capabilities{})) {
				t.Fatalf("omission: %s", raw)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			if tc.name == "all" && string(fields["capabilities"]) != `{"microphone":true,"camera":true,"geolocation":true,"displayCapture":true,"fullscreen":true,"clipboardWrite":true}` {
				t.Fatalf("canonical feature fields: %s", fields["capabilities"])
			}
			var offline bytes.Buffer
			a.serveManifest(&offline)
			if !bytes.Equal(bytes.TrimSpace(offline.Bytes()), raw) {
				t.Fatalf("offline differs from canonical manifest: %s", offline.Bytes())
			}
			var synced wire.SyncRequest
			if err := json.Unmarshal(raw, &synced); err != nil {
				t.Fatal(err)
			}
			if (synced.Capabilities == nil) != (tc.features == (Capabilities{})) || synced.Capabilities != nil && *synced.Capabilities != tc.features {
				t.Fatalf("sync declaration: %+v", synced.Capabilities)
			}
			if first.Capabilities != nil {
				*first.Capabilities = Capabilities{}
				if *a.Manifest().Capabilities != tc.features {
					t.Fatal("manifest aliases agent state")
				}
			}
		})
	}
}

func TestSyncCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name     string
		features Capabilities
	}{
		{"omitted", Capabilities{}},
		{"all", Capabilities{Microphone: true, Camera: true, Geolocation: true, DisplayCapture: true, Fullscreen: true, ClipboardWrite: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, mock := testAgent(t)
			a.capabilities = tc.features
			var offline bytes.Buffer
			a.serveManifest(&offline)
			if err := a.syncWithAirlock(t.Context()); err != nil {
				t.Fatal(err)
			}
			requests := mock.RequestsByPath("/api/agent/sync")
			if len(requests) != 1 {
				t.Fatalf("sync requests: %d", len(requests))
			}
			if !bytes.Equal(requests[0].Body, bytes.TrimSpace(offline.Bytes())) {
				t.Fatalf("runtime/offline mismatch: %s != %s", requests[0].Body, offline.Bytes())
			}
		})
	}
}
