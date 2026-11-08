package wire

// AppCapabilities declares platform-neutral application feature requirements.
// Every feature is default-deny. A declaration does not grant permission or
// guarantee support on the platform hosting the application.
type AppCapabilities struct {
	Microphone     bool `json:"microphone,omitempty"`
	Camera         bool `json:"camera,omitempty"`
	Geolocation    bool `json:"geolocation,omitempty"`
	DisplayCapture bool `json:"displayCapture,omitempty"`
	Fullscreen     bool `json:"fullscreen,omitempty"`
	ClipboardWrite bool `json:"clipboardWrite,omitempty"`
}
