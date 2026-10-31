package agentsdk

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/airlockrun/agentsdk/wire"
	"github.com/google/uuid"
)

func TestUIChangeTopicDefinition(t *testing.T) {
	for _, topic := range []string{"", "Tasks", "tasks.list", "tasks users", "_tasks"} {
		t.Run(topic, func(t *testing.T) {
			a := New(Config{Description: "offline"})
			defer func() {
				if recover() == nil {
					t.Fatal("invalid topic accepted")
				}
			}()
			a.RegisterUIChangeTopic(topic)
		})
	}
	a := New(Config{Description: "offline"})
	h := a.RegisterUIChangeTopic("tasks")
	if h.agent != a || h.topic != "tasks" {
		t.Fatal("handle lost definition")
	}
	manifest := a.Manifest()
	if len(manifest.Routes) != 1 || manifest.Routes[0].Path != wire.UIChangeStreamPath || manifest.Routes[0].Method != http.MethodGet || manifest.Routes[0].Access != wire.AccessUser || manifest.Routes[0].Logging != LoggingOff {
		t.Fatalf("stream declaration = %+v", manifest.Routes)
	}
	if manifest.RuntimeProtocol != wire.AppRuntimeProtocol {
		t.Fatal("changed runtime protocol")
	}
}

func TestUIChangePublishRequiresTransaction(t *testing.T) {
	a, _ := testAgent(t)
	h := a.RegisterUIChangeTopic("tasks")
	for _, tx := range []DBTX{nil, a.DB(), (*sql.Tx)(nil)} {
		if err := h.Publish(context.Background(), tx, uuid.NewString()); err == nil {
			t.Fatal("nontransaction accepted")
		}
	}
}

func TestUIChangeTopicsFreezeAndDuplicate(t *testing.T) {
	for _, freeze := range []bool{false, true} {
		t.Run(map[bool]string{true: "frozen", false: "duplicate"}[freeze], func(t *testing.T) {
			a := New(Config{Description: "offline"})
			a.RegisterUIChangeTopic("tasks")
			if freeze {
				a.Manifest()
			}
			defer func() {
				if recover() == nil {
					t.Fatal("registration accepted")
				}
			}()
			a.RegisterUIChangeTopic("tasks")
		})
	}
}

func TestUIChangeStreamRejectsBrowserAudienceAndInvalidTopics(t *testing.T) {
	a, _ := testAgent(t)
	a.RegisterUIChangeTopic("tasks")
	human := &wire.CallerUser{ID: uuid.NewString(), PlatformMember: true}
	caller := callerFromWire(wire.Caller{Kind: "user", Access: wire.AccessUser, User: human, Initiator: human,
		Origin: wire.CallerOrigin{Interface: "http", Execution: "request"}})
	for _, query := range []string{"", "topic=tasks&user_id=" + uuid.NewString(), "topic=tasks&topic=tasks", "topic=unknown", "topic=tasks,users", "topic=tasks&invalid=%zz"} {
		t.Run(query, func(t *testing.T) {
			r := httptest.NewRequest("GET", wire.UIChangeStreamPath+"?"+query, nil)
			r = r.WithContext(contextWithLazyRun(r.Context(), &lazyRun{agent: a, caller: caller}))
			err := a.streamUIChanges(httptest.NewRecorder(), r)
			var failure *HTTPError
			if !errors.As(err, &failure) || failure.Status != 400 {
				t.Fatalf("invalid subscription = %v", err)
			}
		})
	}
}

func TestUIChangeShutdownInterruptsBlockedListenerStartup(t *testing.T) {
	a, _ := testAgent(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	a.databaseURL = "postgres://fixture:fixture@" + listener.Addr().String() + "/fixture?sslmode=disable"
	accepted := make(chan net.Conn, 1)
	go func() {
		connection, err := listener.Accept()
		if err == nil {
			accepted <- connection
		}
	}()
	subscribed := make(chan error, 1)
	go func() {
		_, unsubscribe, err := a.subscribeUIChanges(t.Context())
		if unsubscribe != nil {
			unsubscribe()
		}
		subscribed <- err
	}()
	select {
	case connection := <-accepted:
		defer connection.Close()
	case <-time.After(3 * time.Second):
		t.Fatal("listener did not connect")
	}
	closed := make(chan error, 1)
	go func() { closed <- a.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown blocked on PostgreSQL startup")
	}
	select {
	case err := <-subscribed:
		if err == nil {
			t.Fatal("closed listener admitted a subscriber")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("subscriber did not settle")
	}
}
