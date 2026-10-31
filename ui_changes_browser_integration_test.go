//go:build airlockplatformintegration

package agentsdk

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/airlockrun/agentsdk/wire"
	"github.com/airlockrun/goai/tool"
	"github.com/google/uuid"
)

// This fixture uses real SDK assets, real PostgreSQL transactions/notifications,
// and the real stream handler. Test-only cookie admission is deliberately local;
// host credentials/deployment authorization are tested in service/execution.
func TestUIChangesBrowser(t *testing.T) {
	node := os.Getenv("AIRLOCK_UI_TEST_NODE")
	if node == "" {
		t.Fatal("AIRLOCK_UI_TEST_NODE must select the external browser test runtime")
	}
	a, changes := uiChangeDatabase(t)
	userA, userB := uuid.NewString(), uuid.NewString()
	mutate := func(ctx context.Context, word string) error {
		user, ok := CallerFromContext(ctx).User()
		if !ok {
			return errors.New("fixture human is required")
		}
		return setUIWord(ctx, a, changes, user.ID, word, false)
	}
	type wordInput struct {
		Word string `json:"word"`
	}
	type wordOutput struct {
		Word string `json:"word"`
	}
	a.RegisterTool(tool.Typed[wordInput, wordOutput]("set_word").Description("Set your word").
		Execute(func(ctx context.Context, input wordInput) (wordOutput, error) {
			return wordOutput{Word: input.Word}, mutate(ctx, input.Word)
		}).Build(), AccessUser)
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/host.js" {
			w.Header().Set("Content-Type", "application/javascript")
			fmt.Fprint(w, `const frame=document.querySelector('iframe');frame.onload=()=>frame.contentWindow.postMessage({type:'airlock:web-ui:v1',action:'probe',nonce:'fixture'},frame.src);`)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!doctype html><html><head><script defer src="/host.js"></script></head><body><h1>Airlock fixture</h1><iframe title="App" src="%s/" style="width:100%%;height:750px;border:0"></iframe></body></html>`, a.promptData.AgentRouteURL)
	}))
	defer host.Close()
	a.promptData.AgentDashboardURL = host.URL
	mux := http.NewServeMux()
	mux.HandleFunc("GET /__air/assets/{name}", a.handleAsset)
	mux.HandleFunc("GET "+wire.UIChangeStreamPath, func(w http.ResponseWriter, r *http.Request) {
		if err := a.streamUIChanges(w, r); err != nil {
			http.Error(w, err.Error(), 500)
		}
	})
	read := func(r *http.Request) string {
		user, _ := CallerFromContext(r.Context()).User()
		word := "Hello"
		if err := a.DB().QueryRowContext(r.Context(), "SELECT word FROM fixture_words WHERE user_id=$1", user.ID).Scan(&word); err != nil && !errors.Is(err, sql.ErrNoRows) {
			t.Error(err)
		}
		return word
	}
	mux.HandleFunc("GET /word", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, html.EscapeString(read(r)))
	})
	write := func(w http.ResponseWriter, r *http.Request) {
		word := r.FormValue("word")
		if err := mutate(r.Context(), word); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
	mux.HandleFunc("POST /word", write)
	mux.HandleFunc("POST /tool/set_word", func(w http.ResponseWriter, r *http.Request) {
		payload, err := json.Marshal(wordInput{Word: r.FormValue("word")})
		if err == nil {
			_, err = a.tools["set_word"].Execute(r.Context(), payload, tool.CallOptions{})
		}
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!doctype html><html><head><meta name="viewport" content="width=device-width"><script src="%s"></script>`, Assets.HTMX)
		if err := WebUI(WebUIOptions{}).Render(r.Context(), w); err != nil {
			t.Error(err)
		}
		fmt.Fprintf(w, `</head><body style="font:20px system-ui;min-height:2000px;padding:24px"><header data-airlock-standalone-only><h1>Welcome</h1></header><main><p>Your word: <span id="word" data-airlock-topic="word" hx-get="/word" hx-include="#filters" hx-trigger="airlock:change" hx-swap="innerHTML">%s</span></p><form id="filters"><label>Filter <input name="filter" value="keep"></label></form><form hx-post="/word" hx-swap="none"><label>Word <input name="word" value="draft"></label><button type="submit">Set word</button></form><p id="stable">Keep this form and your place.</p></main></body></html>`, html.EscapeString(read(r)))
	})
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("fixture_user")
		if err != nil || cookie.Value != userA && cookie.Value != userB {
			http.Error(w, "fixture authentication required", 401)
			return
		}
		mux.ServeHTTP(w, r.WithContext(uiChangeCaller(r.Context(), a, cookie.Value)))
	}))
	defer app.Close()
	a.promptData.AgentRouteURL = app.URL
	command := exec.CommandContext(t.Context(), node, "assets/ui-changes.browser.test.cjs")
	command.Env = append(os.Environ(), "UI_CHANGE_FIXTURE_URL="+app.URL, "UI_CHANGE_HOST_URL="+host.URL,
		"UI_CHANGE_USER_A="+userA, "UI_CHANGE_USER_B="+userB)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("browser: %v\n%s", err, output)
	}
	t.Log(strings.TrimSpace(string(output)))
	deadline := time.Now().Add(3 * time.Second)
	for len(a.uiChangeStreams) != 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(a.uiChangeStreams) != 0 {
		t.Fatalf("browser leaked %d streams", len(a.uiChangeStreams))
	}
}
