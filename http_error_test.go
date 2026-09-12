package agentsdk

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/airlockrun/agentsdk/wire"
)

func TestRouteHTTPErrorWritesSafeResponse(t *testing.T) {
	a, _ := testAgent(t)
	cause := errors.New("database row contained secret details")
	a.RegisterRoute(&Route{
		Method: http.MethodGet,
		Path:   "/thing",
		Handler: func(http.ResponseWriter, *http.Request) error {
			return NewHTTPError(http.StatusNotFound, "thing not found", cause)
		},
		Access:      AccessPublic,
		Description: "Find a thing",
	})

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/thing", nil)
	r.Header.Set("Authorization", "Bearer "+a.token)
	setTestCallerHeader(t, r, testWireCaller("anonymous", wire.AccessPublic))
	a.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
	if got := w.Body.String(); got != "thing not found\n" {
		t.Fatalf("body = %q, want safe public message", got)
	}
	if strings.Contains(w.Body.String(), cause.Error()) {
		t.Fatal("response exposed internal cause")
	}
}

func TestNewHTTPErrorRejectsInvalidDeclaration(t *testing.T) {
	for _, test := range []struct {
		name    string
		status  int
		message string
	}{
		{name: "success status", status: http.StatusOK, message: "bad"},
		{name: "empty message", status: http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			expectPanicContains(t, "NewHTTPError", func() {
				NewHTTPError(test.status, test.message, nil)
			})
		})
	}
}
