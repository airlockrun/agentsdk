package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
)

// newProductionTargetServer adds an explicit deployment contract to existing
// narrow operator response fixtures. No app process or host state is simulated.
func newProductionTargetServer(handler http.Handler) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prefix, suffix, found := strings.Cut(r.URL.Path, "/deployments/production")
		if found && strings.HasPrefix(prefix, "/api/v1/agents/") {
			id := strings.TrimPrefix(prefix, "/api/v1/agents/")
			if suffix == "" && r.Method == http.MethodGet {
				fmt.Fprintf(w, `{"deployment":{"id":%q,"appId":%q,"runtimeAgentId":%q,"name":"production","kind":"production"}}`, id, id, id)
				return
			}
			copy := r.Clone(r.Context())
			copy.URL.Path = prefix + suffix
			handler.ServeHTTP(w, copy)
			return
		}
		handler.ServeHTTP(w, r)
	}))
}
