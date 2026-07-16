// Package handlers holds the HTTP handlers for the agent's templ pages.
// In the MVC split: domain packages own data and business logic (model);
// templ files + view-model types under views/ own presentation (view);
// this package wires them together by converting domain types to view-
// model types and rendering (controller). Handlers may import any domain
// package and views; nothing here imports back out, so no cycle forms.
//
// Add a handler per page or partial: HomeHandler, BoardHandler, etc.
// Register bound methods from main.go via agent.RegisterRoute.
package handlers

import (
	"net/http"

	"agent/views"
)

// Handler owns the HTTP handlers. Add a package-local Deps struct and accept it
// in New when handlers need services from a domain package.
type Handler struct{}

// New constructs the HTTP handler set.
func New() *Handler {
	return &Handler{}
}

// Home renders the default placeholder homepage. The view-model is
// constructed inline because there's no domain yet; replace this with
// a converter from your own data once you add a real model.
func (h *Handler) Home(w http.ResponseWriter, r *http.Request) error {
	view := views.HomeView{
		Title:    "Hello from your new agent",
		Subtitle: "This is the default placeholder homepage.",
		Examples: []string{
			`"Replace this homepage with a dashboard for my tasks"`,
			`"Add a webhook that posts new GitHub issues to Slack"`,
			`"Schedule a daily digest of unread emails"`,
		},
	}
	return views.Index(view).Render(r.Context(), w)
}
