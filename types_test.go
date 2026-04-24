package agentsdk

import "testing"

func TestAuthRequiredError(t *testing.T) {
	err := &AuthRequiredError{
		Slug:     "gmail",
		ConnName: "Gmail",
		AuthURL:  "https://airlock.test/auth/gmail",
	}

	ae, ok := IsAuthRequired(err)
	if !ok {
		t.Fatal("expected IsAuthRequired to return true")
	}
	if ae.Slug != "gmail" {
		t.Fatalf("expected gmail, got %s", ae.Slug)
	}
}

func TestResolveDisplayPart(t *testing.T) {
	// Minimal PNG header (8 bytes).
	pngHeader := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}

	tests := []struct {
		name     string
		input    DisplayPart
		wantType string
		wantMime string
		wantFile string
	}{
		{
			name:     "png bytes infers image/png",
			input:    DisplayPart{Data: pngHeader},
			wantType: "image",
			wantMime: "image/png",
			wantFile: "image.png",
		},
		{
			name:     "unknown bytes default to octet-stream file",
			input:    DisplayPart{Data: []byte{0x00, 0x01, 0x02}},
			wantType: "file",
			wantMime: "application/octet-stream",
			wantFile: "file.bin",
		},
		{
			name:     "explicit mimeType infers type",
			input:    DisplayPart{MimeType: "audio/mpeg"},
			wantType: "audio",
			wantMime: "audio/mpeg",
			wantFile: "audio.mp3",
		},
		{
			name:     "explicit mimeType video",
			input:    DisplayPart{MimeType: "video/mp4"},
			wantType: "video",
			wantMime: "video/mp4",
			wantFile: "video.f4v", // mime.ExtensionsByType returns alphabetically
		},
		{
			name:     "text-only part gets type text",
			input:    DisplayPart{Text: "hello"},
			wantType: "text",
			wantMime: "",
			wantFile: "",
		},
		{
			name:     "explicit type and filename unchanged",
			input:    DisplayPart{Type: "file", Filename: "report.pdf", MimeType: "application/pdf"},
			wantType: "file",
			wantMime: "application/pdf",
			wantFile: "report.pdf",
		},
		{
			name:     "source with explicit type generates filename",
			input:    DisplayPart{Type: "image", Source: "charts/rev.png", MimeType: "image/png"},
			wantType: "image",
			wantMime: "image/png",
			wantFile: "image.png",
		},
		{
			name:     "data with explicit mimeType skips detection",
			input:    DisplayPart{Data: []byte("not really png"), MimeType: "image/png"},
			wantType: "image",
			wantMime: "image/png",
			wantFile: "image.png",
		},
		{
			name:     "empty part stays empty",
			input:    DisplayPart{},
			wantType: "",
			wantMime: "",
			wantFile: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := tt.input
			ResolveDisplayPart(&p)
			if p.Type != tt.wantType {
				t.Errorf("Type = %q, want %q", p.Type, tt.wantType)
			}
			if p.MimeType != tt.wantMime {
				t.Errorf("MimeType = %q, want %q", p.MimeType, tt.wantMime)
			}
			if p.Filename != tt.wantFile {
				t.Errorf("Filename = %q, want %q", p.Filename, tt.wantFile)
			}
		})
	}
}
