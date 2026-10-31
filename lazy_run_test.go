package agentsdk

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/airlockrun/agentsdk/wire"
)

func TestLazyRouteMaterializesOnceAndDiscardsOffDetails(t *testing.T) {
	a, mock := testAgent(t)
	mock.RouteLogging = wire.LoggingOff
	a.RegisterRoute(&Route{Method: "POST", Path: "/work", Access: AccessUser, Description: "Work", Logging: LoggingOff, Handler: func(w http.ResponseWriter, r *http.Request) error {
		if _, ok := CallerFromContext(r.Context()).User(); !ok {
			t.Fatal("caller snapshot missing")
		}
		if len(mock.Requests()) != 0 {
			t.Fatal("caller read performed I/O")
		}
		var wg sync.WaitGroup
		for range 12 {
			wg.Go(func() { a.Logger(r.Context()).Info("sensitive optional text") })
		}
		wg.Wait()
		if len(mock.Requests()) != 0 {
			t.Fatal("Off logging materialized a run")
		}
		active := a.runForCall(r.Context())
		active.recordAction("test", map[string]string{"private": "omit"}, nil, errors.New("omit error"), time.Millisecond)
		if active.caller.Kind() != CallerUser {
			t.Fatal("materialization changed actor")
		}
		return errors.New("omit route error")
	}})
	r := httptest.NewRequest("POST", "/work", nil)
	r.Header.Set("Authorization", "Bearer "+a.token)
	setTestCallerHeader(t, r, testWireCaller("user", wire.AccessUser))
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	if len(mock.RequestsByPath("/api/agent/run/materialize-route")) != 1 {
		t.Fatal("concurrent APIs materialized multiple runs")
	}
	completion := completedRun(t, mock)
	if completion.Status != "error" || len(completion.Actions) != 0 || len(completion.Logs) != 0 || completion.Error != "" || completion.PanicTrace != "" {
		t.Fatalf("Off retained optional audit: %+v", completion)
	}
}

func TestRouteHandlerUsesAbsoluteAdmissionDeadline(t *testing.T) {
	a, mock := testAgent(t)
	deadline := time.Now().Add(time.Minute).UTC()
	a.RegisterRoute(&Route{Method: "GET", Path: "/deadline", Access: AccessPublic, Description: "Deadline", Handler: func(w http.ResponseWriter, r *http.Request) error {
		got, ok := r.Context().Deadline()
		if !ok || !got.Equal(deadline) {
			t.Fatalf("deadline=%v %v want %v", got, ok, deadline)
		}
		return nil
	}})
	r := httptest.NewRequest("GET", "/deadline", nil)
	r.Header.Set("Authorization", "Bearer "+a.token)
	setTestCallerHeader(t, r, testWireCaller("anonymous", wire.AccessPublic))
	r.Header.Set(wire.RouteDeadlineHeader, deadline.Format(time.RFC3339Nano))
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	if w.Code != 200 || len(mock.Requests()) != 0 {
		t.Fatalf("response=%d requests=%d", w.Code, len(mock.Requests()))
	}
	l := &lazyRun{agent: a, caller: callerFromWire(testWireCaller("user", wire.AccessUser))}
	if err := func() (err error) {
		defer func() {
			if recover() != nil {
				err = errors.New("rejected")
			}
		}()
		l.get(context.Background())
		return nil
	}(); err == nil {
		t.Fatal("missing proof fell back to app authority")
	}
	if len(mock.RequestsByPath("/api/agent/run/create")) != 0 {
		t.Fatal("missing proof created application run")
	}
}
