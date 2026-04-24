package agentsdk

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEventWriterProgress(t *testing.T) {
	w := httptest.NewRecorder()
	ew := newEventWriter(w)

	if err := ew.WriteProgress("step 1 done"); err != nil {
		t.Fatal(err)
	}

	body := w.Body.String()
	if !strings.Contains(body, `"type":"progress"`) {
		t.Fatalf("expected progress event, got: %s", body)
	}
	if !strings.Contains(body, `"message":"step 1 done"`) {
		t.Fatalf("expected message, got: %s", body)
	}
	if w.Header().Get("Content-Type") != "application/x-ndjson" {
		t.Fatalf("expected ndjson content type, got %s", w.Header().Get("Content-Type"))
	}
}
