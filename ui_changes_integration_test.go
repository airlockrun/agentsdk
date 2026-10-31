//go:build airlockplatformintegration

package agentsdk

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/airlockrun/agentsdk/wire"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

func uiChangeDatabase(t *testing.T) (*Agent, *UIChangeTopic) {
	t.Helper()
	dsn := os.Getenv("AIRLOCK_PLATFORM_TEST_DB_URL")
	if dsn == "" {
		t.Fatal("explicit disposable AIRLOCK_PLATFORM_TEST_DB_URL is required")
	}
	root, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := "ui_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := root.ExecContext(t.Context(), "CREATE SCHEMA "+pq.QuoteIdentifier(schema)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = root.ExecContext(context.Background(), "DROP SCHEMA "+pq.QuoteIdentifier(schema)+" CASCADE")
		root.Close()
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	query := u.Query()
	query.Set("search_path", schema)
	query.Set("connect_timeout", "3")
	query.Set("application_name", schema)
	u.RawQuery = query.Encode()
	database, err := sql.Open("postgres", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	a := New(Config{Description: "UI changes fixture"})
	h := a.RegisterUIChangeTopic("word")
	a.phase, a.agentID, a.databaseURL, a.db.db = agentRunning, uuid.NewString(), u.String(), database
	t.Cleanup(func() {
		if a.runtimeAvailable() {
			if err := a.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	if err := a.initializeUIChanges(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(t.Context(), `CREATE TABLE fixture_words (user_id uuid PRIMARY KEY, word text NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	return a, h
}

func uiChangeCaller(ctx context.Context, a *Agent, user string) context.Context {
	human := &wire.CallerUser{ID: user, PlatformMember: true}
	return contextWithLazyRun(ctx, &lazyRun{agent: a, caller: callerFromWire(wire.Caller{Kind: "user", Access: wire.AccessUser,
		User: human, Initiator: human, Origin: wire.CallerOrigin{Interface: "http", Execution: "request"}})})
}

func setUIWord(ctx context.Context, a *Agent, h *UIChangeTopic, user, word string, rollback bool) error {
	return a.DB().Transaction(ctx, nil, func(tx DBTX) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO fixture_words (user_id, word) VALUES ($1,$2)
			ON CONFLICT (user_id) DO UPDATE SET word=EXCLUDED.word`, user, word); err != nil {
			return err
		}
		if err := h.Publish(ctx, tx, user); err != nil {
			return err
		}
		if rollback {
			return errors.New("intentional rollback")
		}
		return nil
	})
}

func openUIChangeStream(t *testing.T, a *Agent, user string) (<-chan wire.UIChange, context.CancelFunc) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(uiChangeCaller(r.Context(), a, user))
		if err := a.streamUIChanges(w, r); err != nil && r.Context().Err() == nil {
			t.Errorf("stream: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	ctx, cancel := context.WithCancel(t.Context())
	req, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"?topic=word", nil)
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		cancel()
		t.Fatalf("stream status %d", response.StatusCode)
	}
	events := make(chan wire.UIChange, 64)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer response.Body.Close()
		scanner := bufio.NewScanner(response.Body)
		for scanner.Scan() {
			if !strings.HasPrefix(scanner.Text(), "data: ") {
				continue
			}
			var event wire.UIChange
			if err := json.Unmarshal([]byte(strings.TrimPrefix(scanner.Text(), "data: ")), &event); err != nil {
				return
			}
			select {
			case events <- event:
			case <-ctx.Done():
				return
			}
		}
	}()
	stop := func() { cancel(); <-done }
	t.Cleanup(stop)
	return events, stop
}

func uiChangeNext(t *testing.T, events <-chan wire.UIChange) wire.UIChange {
	t.Helper()
	select {
	case event := <-events:
		return event
	case <-time.After(5 * time.Second):
		t.Fatal("missing change")
		return wire.UIChange{}
	}
}

func TestUIChangesCommitRollbackReplicasAndRestart(t *testing.T) {
	a, h := uiChangeDatabase(t)
	userA, userB := uuid.NewString(), uuid.NewString()
	first, stop := openUIChangeStream(t, a, userA)
	second, _ := openUIChangeStream(t, a, userA)
	other, _ := openUIChangeStream(t, a, userB)
	for _, events := range []<-chan wire.UIChange{first, second, other} {
		if event := uiChangeNext(t, events); event.Topic != "word" || event.Revision != "" {
			t.Fatalf("initial = %+v", event)
		}
	}
	if err := setUIWord(t.Context(), a, h, userA, "rolled-back", true); err == nil {
		t.Fatal("rollback committed")
	}
	var count int
	if err := a.DB().QueryRowContext(t.Context(), "SELECT count(*) FROM _airlock_ui_changes_v1").Scan(&count); err != nil || count != 0 {
		t.Fatalf("rollback intent remains: %d, %v", count, err)
	}
	select {
	case event := <-first:
		t.Fatalf("rollback event: %+v", event)
	case <-time.After(150 * time.Millisecond):
	}
	if err := setUIWord(t.Context(), a, h, userA, "updated", false); err != nil {
		t.Fatal(err)
	}
	changed := uiChangeNext(t, first)
	if event := uiChangeNext(t, second); event != changed || event.Revision == "" {
		t.Fatalf("replica read = %+v, expected %+v", event, changed)
	}
	select {
	case event := <-other:
		t.Fatalf("other user's event: %+v", event)
	case <-time.After(150 * time.Millisecond):
	}
	stop()
	// A new SDK process object uses the same durable app database and identity.
	restarted := New(Config{Description: "restarted"})
	restarted.RegisterUIChangeTopic("word")
	restarted.phase, restarted.agentID, restarted.databaseURL, restarted.db.db = agentRunning, a.agentID, a.databaseURL, a.db.db
	t.Cleanup(restarted.stopUIChanges)
	if err := restarted.initializeUIChanges(t.Context()); err != nil {
		t.Fatal(err)
	}
	resume, _ := openUIChangeStream(t, restarted, userA)
	if event := uiChangeNext(t, resume); event != changed {
		t.Fatalf("restart lost revision: %+v", event)
	}
	var word string
	if err := restarted.DB().QueryRowContext(t.Context(), "SELECT word FROM fixture_words WHERE user_id=$1", userA).Scan(&word); err != nil || word != "updated" {
		t.Fatalf("word = %q, %v", word, err)
	}
	// Duplicate notifications do not produce repeated revisions.
	if _, err := a.DB().ExecContext(t.Context(), "SELECT pg_notify($1, '')", a.uiChangeChannel()); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-resume:
		t.Fatalf("duplicate event: %+v", event)
	case <-time.After(150 * time.Millisecond):
	}
	if err := setUIWord(t.Context(), a, h, userA, "cross-replica", false); err != nil {
		t.Fatal(err)
	}
	latest := uiChangeNext(t, resume)
	if original := uiChangeNext(t, second); original != latest || latest.Revision == changed.Revision {
		t.Fatalf("independent SDK listeners diverged: %+v, %+v", original, latest)
	}
}

func TestUIChangesConcurrentTransactionsAndDeploymentIsolation(t *testing.T) {
	a, h := uiChangeDatabase(t)
	preview, previewTopic := uiChangeDatabase(t)
	user := uuid.NewString()
	if err := a.DB().Transaction(t.Context(), nil, func(tx DBTX) error {
		if err := previewTopic.Publish(t.Context(), tx, user); err == nil {
			t.Fatal("another deployment's transaction accepted")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	events, _ := openUIChangeStream(t, a, user)
	previewEvents, _ := openUIChangeStream(t, preview, user)
	uiChangeNext(t, events)
	uiChangeNext(t, previewEvents)
	var workers sync.WaitGroup
	for i := 0; i < 12; i++ {
		workers.Go(func() {
			if err := setUIWord(t.Context(), a, h, user, fmt.Sprint(i), false); err != nil {
				t.Error(err)
			}
		})
	}
	workers.Wait()
	var revision string
	if err := a.DB().QueryRowContext(t.Context(), "SELECT revision::text FROM _airlock_ui_changes_v1 WHERE user_id=$1", user).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	for uiChangeNext(t, events).Revision != revision {
	}
	select {
	case event := <-previewEvents:
		t.Fatalf("production crossed preview: %+v", event)
	case <-time.After(150 * time.Millisecond):
	}
	if err := setUIWord(t.Context(), preview, previewTopic, user, "preview", false); err != nil {
		t.Fatal(err)
	}
	if event := uiChangeNext(t, previewEvents); event.Revision == revision {
		t.Fatal("deployment revision reused")
	}
}

func TestUIChangesListenerReconnectAndShutdown(t *testing.T) {
	a, h := uiChangeDatabase(t)
	user := uuid.NewString()
	events, _ := openUIChangeStream(t, a, user)
	uiChangeNext(t, events)
	u, err := url.Parse(a.databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	var pid int
	if err := a.DB().QueryRowContext(t.Context(), `SELECT pid FROM pg_stat_activity WHERE application_name=$1 AND query LIKE 'LISTEN %'`, u.Query().Get("application_name")).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err := a.DB().ExecContext(t.Context(), "SELECT pg_terminate_backend($1)", pid); err != nil {
		t.Fatal(err)
	}
	if err := setUIWord(t.Context(), a, h, user, "during reconnect", false); err != nil {
		t.Fatal(err)
	}
	if event := uiChangeNext(t, events); event.Revision == "" {
		t.Fatal("reconnect lost durable intent")
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for len(a.uiChangeStreams) != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(a.uiChangeStreams) != 0 {
		t.Fatal("Close retained live streams")
	}
}
