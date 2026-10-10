package mockairlock

import (
	"github.com/airlockrun/agentsdk/localruntime"
	"github.com/airlockrun/goai/stream"
	"net/http"
)

func NewWithModels(models map[string]stream.Model) (*Mock, string) {
	if len(models) == 0 {
		panic("mockairlock: explicit streaming models are required")
	}
	for _, model := range models {
		if model == nil {
			panic("mockairlock: nil streaming model")
		}
	}
	return newMock(models)
}

func (m *Mock) streamModel(w http.ResponseWriter, r *http.Request) {
	localruntime.ModelHandler(m.models).ServeHTTP(w, r)
}
