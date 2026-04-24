package agentsdk

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/dop251/goja"
)

// ConversationVM holds persistent state for a single conversation.
// The goja VM is recreated per-run, but the store map survives across prompts.
type ConversationVM struct {
	mu              sync.Mutex
	store           map[string]any
	storeSize       int64
	lastUsed        time.Time
	serializedFuncs map[string]string // key → function source text
	needsCleanup    bool
}

// ConversationVMConfig configures conversation VM behavior.
type ConversationVMConfig struct {
	IdleTimeout     time.Duration // default: 72h
	MaxStoreSize    int64         // default: 4MB
	GCSweepInterval time.Duration // default: 1h
}

// DefaultConversationVMConfig returns sensible defaults.
func DefaultConversationVMConfig() ConversationVMConfig {
	return ConversationVMConfig{
		IdleTimeout:     72 * time.Hour,
		MaxStoreSize:    4 << 20, // 4MB
		GCSweepInterval: time.Hour,
	}
}

// getOrCreateConvVM returns the ConversationVM for the given conversation.
// Returns nil if conversationID is empty (webhook/cron runs have no conversation).
func (a *Agent) getOrCreateConvVM(conversationID string) *ConversationVM {
	if conversationID == "" {
		return nil
	}
	if v, ok := a.conversationVMs.Load(conversationID); ok {
		cvm := v.(*ConversationVM)
		cvm.mu.Lock()
		cvm.lastUsed = time.Now()
		cvm.mu.Unlock()
		return cvm
	}
	cvm := &ConversationVM{
		store:           make(map[string]any),
		serializedFuncs: make(map[string]string),
		lastUsed:        time.Now(),
	}
	actual, _ := a.conversationVMs.LoadOrStore(conversationID, cvm)
	return actual.(*ConversationVM)
}

// startConvVMGC starts a background goroutine that sweeps idle ConversationVM entries.
func (a *Agent) startConvVMGC(cfg ConversationVMConfig) {
	go func() {
		ticker := time.NewTicker(cfg.GCSweepInterval)
		defer ticker.Stop()
		for range ticker.C {
			cutoff := time.Now().Add(-cfg.IdleTimeout)
			a.conversationVMs.Range(func(key, value any) bool {
				cvm := value.(*ConversationVM)
				cvm.mu.Lock()
				idle := cvm.lastUsed.Before(cutoff)
				cvm.mu.Unlock()
				if idle {
					a.conversationVMs.Delete(key)
				}
				return true
			})
		}
	}()
}

// teardownStore exports the VM's store back to the ConversationVM after a run completes.
// Serializes functions via .toString() for re-evaluation in the next VM, and measures store size.
func (r *run) teardownStore() {
	if r.convVM == nil || r.vm == nil {
		return
	}

	r.convVM.mu.Lock()
	defer r.convVM.mu.Unlock()

	storeVal := r.vm.Get("store")
	if storeVal == nil || goja.IsUndefined(storeVal) {
		return
	}
	obj := storeVal.ToObject(r.vm)

	newStore := make(map[string]any)
	newFuncs := make(map[string]string)

	for _, key := range obj.Keys() {
		val := obj.Get(key)
		if val == nil || goja.IsUndefined(val) {
			continue
		}

		// Check if value is callable (function).
		if _, ok := goja.AssertFunction(val); ok {
			// Serialize via .toString().
			toString, ok2 := goja.AssertFunction(val.ToObject(r.vm).Get("toString"))
			if ok2 {
				src, err := toString(val)
				if err == nil {
					newFuncs[key] = src.String()
				}
			}
			continue // don't store goja function in Go map
		}

		newStore[key] = val.Export()
	}

	r.convVM.store = newStore
	r.convVM.serializedFuncs = newFuncs

	// Measure size.
	data, err := json.Marshal(newStore)
	if err == nil {
		r.convVM.storeSize = int64(len(data))
	}

	// Check threshold.
	if r.convVM.storeSize > r.agent.convVMConfig.MaxStoreSize && r.agent.convVMConfig.MaxStoreSize > 0 {
		if r.convVM.needsCleanup {
			// Already asked once and still over — hard reset.
			r.convVM.store = make(map[string]any)
			r.convVM.serializedFuncs = make(map[string]string)
			r.convVM.storeSize = 0
			r.convVM.needsCleanup = false
		} else {
			r.convVM.needsCleanup = true
		}
	} else {
		r.convVM.needsCleanup = false
	}
}
