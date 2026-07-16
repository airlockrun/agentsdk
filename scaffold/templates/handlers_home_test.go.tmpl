package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHomeRenders(t *testing.T) {
	handler := New()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)

	if err := handler.Home(w, r); err != nil {
		t.Fatalf("Home: %v", err)
	}
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if w.Body.Len() == 0 {
		t.Fatal("rendered body is empty")
	}
}
