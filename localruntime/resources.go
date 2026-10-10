package localruntime

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/airlockrun/agentsdk/wire"
)

// Resources is the credential-free resource boundary. The adapter owns remote
// authentication and uses the admitted actor, never untrusted callback headers.
type Resources interface {
	Connection(context.Context, wire.Caller, string, wire.ProxyRequest) (*http.Response, error)
	MCP(context.Context, wire.Caller, string, wire.MCPToolCallRequest) (wire.MCPToolCallResponse, error)
}

func (h *Host) resourceCaller(r *http.Request, kind, slug string) (caller wire.Caller, err error) {
	err = h.config.State.update(r.Context(), func(d *stateData) error {
		if r.Header.Get("X-Airlock-Run-ID") == "" {
			caller = appCaller("background")
		} else {
			run := d.Runs[r.Header.Get("X-Airlock-Run-ID")]
			if run == nil || run.Closed {
				return errors.New("localruntime: resource run is closed")
			}
			caller = clone(run.Scope.Caller)
		}
		found := false
		mode := wire.BindingMode("")
		noAuth := false
		if kind == "connection" {
			for _, need := range d.Manifest.Connections {
				if need.Slug == slug {
					found = true
					mode = need.BindingMode
					noAuth = need.AuthMode == wire.ConnectionAuthNone
				}
			}
		} else {
			for _, need := range d.Manifest.MCPServers {
				if need.Slug == slug {
					found = true
					mode = need.BindingMode
					noAuth = need.AuthMode == wire.MCPAuthNone
				}
			}
		}
		if !found {
			return errors.New("localruntime: resource need is not declared")
		}
		if caller.Kind == "user" && !noAuth {
			run := d.Runs[r.Header.Get("X-Airlock-Run-ID")]
			if run.ResourceSession == "" || run.ResourceSession != h.config.ResourceSession {
				return errors.New("localruntime: resource origin session is not the active admitted session")
			}
		}
		if mode == wire.BindingPerUser && caller.Kind != "user" {
			return errors.New("localruntime: per-user resource requires an acting human")
		}
		return nil
	})
	return
}

func (h *Host) resourceProof(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Airlock-Run-ID") == "" {
			if r.Header.Get(wire.InvocationTokenHeader) != "" {
				http.Error(w, "localruntime: receipt requires a run", 403)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		h.receipt(next).ServeHTTP(w, r)
	})
}

func (h *Host) proxyConnection(w http.ResponseWriter, r *http.Request) {
	adapter, err := h.resourceAdapter(r.Context(), "connection", r.PathValue("slug"))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if adapter == nil {
		http.Error(w, "localruntime: connection requires an explicit resource adapter", 501)
		return
	}
	caller, err := h.resourceCaller(r, "connection", r.PathValue("slug"))
	if err != nil {
		http.Error(w, err.Error(), 403)
		return
	}
	var req wire.ProxyRequest
	if err := strictJSON(r.Body, &req); err != nil {
		fail(w, err)
		return
	}
	response, err := adapter.Connection(r.Context(), caller, r.PathValue("slug"), req)
	if err != nil {
		http.Error(w, "localruntime: resource request rejected: "+err.Error(), 502)
		return
	}
	if response == nil || response.Body == nil {
		http.Error(w, "localruntime: resource adapter returned no response", 502)
		return
	}
	defer response.Body.Close()
	for key, values := range response.Header {
		if key == "Authorization" || key == "Set-Cookie" {
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.Header().Set(wire.ConnectionResponseSourceHeader, string(wire.ConnectionResponseSourceUpstream))
	w.WriteHeader(response.StatusCode)
	io.Copy(w, response.Body)
}
func (h *Host) callMCP(w http.ResponseWriter, r *http.Request) {
	adapter, err := h.resourceAdapter(r.Context(), "mcp", r.PathValue("slug"))
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if adapter == nil {
		http.Error(w, "localruntime: MCP requires an explicit resource adapter", 501)
		return
	}
	caller, err := h.resourceCaller(r, "mcp", r.PathValue("slug"))
	if err != nil {
		http.Error(w, err.Error(), 403)
		return
	}
	var req wire.MCPToolCallRequest
	if err := strictJSON(r.Body, &req); err != nil {
		fail(w, err)
		return
	}
	response, err := adapter.MCP(r.Context(), caller, r.PathValue("slug"), req)
	if err != nil {
		http.Error(w, "localruntime: resource request rejected: "+err.Error(), 502)
		return
	}
	writeJSON(w, response)
}

func (h *Host) resourceAdapter(ctx context.Context, kind, slug string) (adapter Resources, err error) {
	err = h.config.State.update(ctx, func(d *stateData) error {
		if kind == "connection" {
			for _, need := range d.Manifest.Connections {
				if need.Slug == slug {
					adapter = h.config.Resources
					if need.AuthMode == wire.ConnectionAuthNone {
						adapter = h.config.NoAuthResources
					}
					return nil
				}
			}
		}
		if kind == "mcp" {
			for _, need := range d.Manifest.MCPServers {
				if need.Slug == slug {
					adapter = h.config.Resources
					if need.AuthMode == wire.MCPAuthNone {
						adapter = h.config.NoAuthResources
					}
					return nil
				}
			}
		}
		return errors.New("localruntime: resource need is not declared")
	})
	return
}
