package mockairlock

import (
	"github.com/airlockrun/agentsdk/localruntime"
	"net/http"
)

func (m *Mock) SetStorage(storage *FileStorage) {
	if storage == nil {
		panic("mockairlock: SetStorage requires storage")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.storage = storage
}

func (m *Mock) recordStorage(r *http.Request) {
	m.mu.Lock()
	if m.storage != nil {
		m.requests = append(m.requests, Request{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone()})
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()
	m.record(r)
}

func (m *Mock) storageRequest(w http.ResponseWriter, r *http.Request, op string) bool {
	m.mu.Lock()
	store := m.storage
	m.mu.Unlock()
	if store == nil {
		return false
	}
	localruntime.StorageHandler(store, op).ServeHTTP(w, r)
	return true
}
