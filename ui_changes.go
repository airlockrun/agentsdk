package agentsdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/airlockrun/agentsdk/wire"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"go.uber.org/zap"
)

var uiChangeTopicPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// Signals are disposable wakeups, not durable delivery/coordination state. One
// LISTEN session serves all streams in this SDK process; every reader uses the
// shared database's authoritative revisions after subscribing.
type uiChangeSignals struct {
	once        sync.Once
	ready       chan struct{}
	done        chan struct{}
	err         error
	mu          sync.Mutex
	subscribers map[chan struct{}]struct{}
}

func newUIChangeSignals() *uiChangeSignals {
	return &uiChangeSignals{ready: make(chan struct{}), done: make(chan struct{}), subscribers: make(map[chan struct{}]struct{})}
}

func (a *Agent) stopUIChanges() {
	a.uiChangesCancel()
	signals := a.uiChangeSignals
	signals.once.Do(func() {
		signals.err = context.Canceled
		close(signals.ready)
		close(signals.done)
	})
	<-signals.done
}

func (a *Agent) subscribeUIChanges(ctx context.Context) (<-chan struct{}, func(), error) {
	signals := a.uiChangeSignals
	wake := make(chan struct{}, 1)
	signals.mu.Lock()
	signals.subscribers[wake] = struct{}{}
	signals.mu.Unlock()
	unsubscribe := func() { signals.mu.Lock(); delete(signals.subscribers, wake); signals.mu.Unlock() }
	signals.once.Do(func() { go a.relayUIChanges(signals) })
	select {
	case <-ctx.Done():
		unsubscribe()
		return nil, nil, ctx.Err()
	case <-signals.ready:
		if signals.err != nil {
			unsubscribe()
			return nil, nil, signals.err
		}
		return wake, unsubscribe, nil
	}
}

func (a *Agent) relayUIChanges(signals *uiChangeSignals) {
	defer close(signals.done)
	listener := pq.NewDialListener(uiChangeDialer{ctx: a.uiChangesContext}, a.databaseURL, time.Second, 30*time.Second, nil)
	stop := context.AfterFunc(a.uiChangesContext, func() { _ = listener.Close() })
	defer stop()
	defer listener.Close()
	signals.err = listener.Listen(a.uiChangeChannel())
	close(signals.ready)
	if signals.err != nil {
		return
	}
	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-a.uiChangesContext.Done():
			return
		case <-listener.Notify:
			signals.mu.Lock()
			for wake := range signals.subscribers {
				select {
				case wake <- struct{}{}:
				default:
				}
			}
			signals.mu.Unlock()
		case <-heartbeat.C:
			if err := listener.Ping(); err != nil && a.uiChangesContext.Err() == nil {
				agentLogger().Warn("UI change listener disconnected; reconnecting", zap.Error(err))
			}
		}
	}
}

// Listener methods serialize connection setup and pings under their own lock.
// Closing the actual socket on shutdown also interrupts those blocked methods.
type uiChangeDialer struct{ ctx context.Context }

func (d uiChangeDialer) Dial(network, address string) (net.Conn, error) {
	return d.DialTimeout(network, address, 5*time.Second)
}

func (d uiChangeDialer) DialTimeout(network, address string, timeout time.Duration) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: timeout, KeepAliveConfig: net.KeepAliveConfig{Enable: true,
		Idle: 20 * time.Second, Interval: 5 * time.Second, Count: 3}}
	connection, err := dialer.DialContext(d.ctx, network, address)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(d.ctx, func() { _ = connection.Close() })
	return &uiChangeConnection{Conn: connection, stop: stop}, nil
}

type uiChangeConnection struct {
	net.Conn
	stop func() bool
}

func (c *uiChangeConnection) Close() error {
	c.stop()
	return c.Conn.Close()
}

// UIChangeTopic is a declared, user-scoped UI invalidation channel. It carries
// only a topic and opaque revision; authenticated fragment routes serve data.
type UIChangeTopic struct {
	agent *Agent
	topic string
}

// RegisterUIChangeTopic declares a user-scoped invalidation topic without I/O.
// Pass the returned late-bound handle into the shared domain service used by
// HTTP handlers, tools and jobs. Topic names match [a-z][a-z0-9_-]{0,63}.
func (a *Agent) RegisterUIChangeTopic(topic string) *UIChangeTopic {
	done := a.beginRegistration("RegisterUIChangeTopic")
	defer done()
	if !uiChangeTopicPattern.MatchString(topic) {
		panic("agentsdk: invalid UI change topic: " + topic)
	}
	if _, exists := a.uiChangeTopics[topic]; exists {
		panic("agentsdk: duplicate UI change topic: " + topic)
	}
	if len(a.uiChangeTopics) == 32 {
		panic("agentsdk: at most 32 UI change topics may be declared")
	}
	a.uiChangeTopics[topic] = struct{}{}
	return &UIChangeTopic{agent: a, topic: topic}
}

// Publish persists an invalidation in the SAME app database transaction as the
// mutation. tx must be the callback value from the owning AgentDB.Transaction,
// not the pool. Returning its error from the callback rolls back both writes.
// userID is the explicit affected human, including for trusted app-owned jobs;
// it does not establish caller authority. Never substitute the application owner.
func (t *UIChangeTopic) Publish(ctx context.Context, tx DBTX, userID string) error {
	if t == nil || t.agent == nil {
		panic("agentsdk: UI change topic is required")
	}
	t.agent.requireRuntime("UIChangeTopic.Publish")
	transaction, ok := tx.(*appTransaction)
	if !ok || transaction == nil || transaction.agent != t.agent {
		return errors.New("agentsdk: UI change publishing requires the app transaction callback value")
	}
	user, err := uuid.Parse(userID)
	if err != nil || user == uuid.Nil {
		return errors.New("agentsdk: UI change publishing requires an affected human UUID")
	}
	_, err = transaction.ExecContext(ctx, `INSERT INTO _airlock_ui_changes_v1 (user_id, topic, revision)
		VALUES ($1, $2, $3) ON CONFLICT (user_id, topic) DO UPDATE SET revision = EXCLUDED.revision`, user.String(), t.topic, uuid.NewString())
	if err != nil {
		return err
	}
	// PostgreSQL delivers NOTIFY only after commit. The durable revision, not the
	// notification, is authoritative; reconnect readers take a fresh snapshot.
	_, err = transaction.ExecContext(ctx, `SELECT pg_notify($1, '')`, t.agent.uiChangeChannel())
	return err
}

func (a *Agent) uiChangeChannel() string {
	return "airlock_ui_" + strings.ReplaceAll(a.agentID, "-", "")
}

func (a *Agent) initializeUIChanges(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, migrationTimeout)
	defer cancel()
	tx, err := a.db.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLockKey(a.agentID)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS _airlock_ui_changes_v1 (
		user_id uuid NOT NULL, topic text NOT NULL, revision uuid NOT NULL,
		PRIMARY KEY (user_id, topic))`); err != nil {
		return err
	}
	return tx.Commit()
}

func (a *Agent) uiChangeRoute() *Route {
	return &Route{Method: http.MethodGet, Path: wire.UIChangeStreamPath, Access: AccessUser,
		Logging: LoggingOff, Description: "Authenticated user-scoped UI invalidations", Handler: a.streamUIChanges}
}

func (a *Agent) streamUIChanges(w http.ResponseWriter, r *http.Request) (retErr error) {
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(a.uiChangesContext, cancel)
	defer stop()
	defer cancel()
	defer func() {
		if ctx.Err() != nil {
			retErr = nil
		}
	}()
	r = r.WithContext(ctx)
	user, ok := CallerFromContext(r.Context()).User()
	if !ok {
		return &HTTPError{Status: http.StatusUnauthorized, Message: "UI changes require a human"}
	}
	if len(r.URL.RawQuery) > 32*(len("topic=")+64+1) {
		return &HTTPError{Status: http.StatusBadRequest, Message: "too many topics"}
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return &HTTPError{Status: http.StatusBadRequest, Message: "invalid topic query"}
	}
	topics := query["topic"]
	if len(topics) == 0 || len(topics) > 32 || len(query) != 1 {
		return &HTTPError{Status: http.StatusBadRequest, Message: "provide 1–32 declared topics only"}
	}
	seen := make(map[string]bool, len(topics))
	for _, topic := range topics {
		if _, exists := a.uiChangeTopics[topic]; !exists || seen[topic] {
			return &HTTPError{Status: http.StatusBadRequest, Message: "unknown or duplicate topic"}
		}
		seen[topic] = true
	}
	sort.Strings(topics)
	// This is replica-local resource backpressure, not audience/coordination
	// state. Durable revisions remain in PostgreSQL when capacity is exhausted.
	select {
	case a.uiChangeStreams <- struct{}{}:
		defer func() { <-a.uiChangeStreams }()
	default:
		return &HTTPError{Status: http.StatusServiceUnavailable, Message: "UI change stream capacity reached"}
	}
	// LISTEN precedes the snapshot, closing the read/subscribe window. Listener
	// reconnection emits nil, causing another durable snapshot, not lost intent.
	wake, unsubscribe, err := a.subscribeUIChanges(ctx)
	if err != nil {
		return err
	}
	defer unsubscribe()
	controller := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	revisions := make(map[string]string, len(topics))
	snapshot := func() error {
		rows, err := a.db.db.QueryContext(r.Context(), `SELECT topic, revision::text FROM _airlock_ui_changes_v1
			WHERE user_id = $1 AND topic = ANY($2)`, user.ID, pq.Array(topics))
		if err != nil {
			return err
		}
		defer rows.Close()
		current := make(map[string]string, len(topics))
		for rows.Next() {
			var topic, revision string
			if err := rows.Scan(&topic, &revision); err != nil {
				return err
			}
			current[topic] = revision
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if err := controller.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
			return err
		}
		for _, topic := range topics {
			previous, initialized := revisions[topic]
			if initialized && previous == current[topic] {
				continue
			}
			data, err := json.Marshal(wire.UIChange{Topic: topic, Revision: current[topic]})
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintf(w, "event: airlock:change\ndata: %s\n\n", data); err != nil {
				return err
			}
			revisions[topic] = current[topic]
		}
		if err := controller.Flush(); err != nil {
			return err
		}
		return controller.SetWriteDeadline(time.Time{})
	}
	if err := snapshot(); err != nil {
		return err
	}
	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return nil
		case <-a.uiChangeSignals.done:
			return errors.New("agentsdk: UI change listener stopped")
		case <-wake:
			// Coalesce notification bursts without periodic backend reads.
			timer := time.NewTimer(50 * time.Millisecond)
			select {
			case <-r.Context().Done():
				timer.Stop()
				return nil
			case <-timer.C:
			}
			drain := true
			for drain {
				select {
				case <-wake:
				default:
					drain = false
				}
			}
			if err := snapshot(); err != nil {
				return err
			}
		case <-heartbeat.C:
			if err := controller.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
				return err
			}
			if _, err := fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return err
			}
			if err := controller.Flush(); err != nil {
				return err
			}
			if err := controller.SetWriteDeadline(time.Time{}); err != nil {
				return err
			}
		}
	}
}
