package localruntime

import (
	"errors"
	"github.com/airlockrun/agentsdk/wire"
	"net/http"
)

func (h *Host) envVar(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	err := h.config.State.update(r.Context(), func(d *stateData) error {
		for _, decl := range d.Manifest.EnvVars {
			if decl.Slug == slug {
				return nil
			}
		}
		return errors.New("localruntime: environment slot is not declared")
	})
	if err != nil {
		http.Error(w, err.Error(), 404)
		return
	}
	value, ok := h.config.EnvVars[slug]
	if !ok {
		http.Error(w, "localruntime: environment slot is not configured", 404)
		return
	}
	writeJSON(w, wire.EnvVarValueResponse{Value: value})
}
