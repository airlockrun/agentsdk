package agentsdk

import (
	"context"
	"testing"
	"time"
)

func TestConversationVM(t *testing.T) {
	t.Run("store survives across runs", func(t *testing.T) {
		a, _ := testAgent(t)

		// Run 1: set store.x = 42
		r1 := newRun(a, "r1", "", "c1", context.Background())
		_, err := executeJS(r1.vmRuntime(), `store.x = 42`)
		if err != nil {
			t.Fatal(err)
		}
		r1.teardownStore()

		// Run 2: fresh VM, same conversation — store.x should be 42
		r2 := newRun(a, "r2", "", "c1", context.Background())
		result, err := executeJS(r2.vmRuntime(), `store.x`)
		if err != nil {
			t.Fatal(err)
		}
		if result != "42" {
			t.Fatalf("expected 42, got %s", result)
		}
		r2.teardownStore()
	})

	t.Run("separate conversations are isolated", func(t *testing.T) {
		a, _ := testAgent(t)

		// c1: set store.x = 1
		r1 := newRun(a, "r1", "", "c1", context.Background())
		executeJS(r1.vmRuntime(), `store.x = 1`)
		r1.teardownStore()

		// c2: store.x should be undefined
		r2 := newRun(a, "r2", "", "c2", context.Background())
		result, err := executeJS(r2.vmRuntime(), `typeof store.x`)
		if err != nil {
			t.Fatal(err)
		}
		if result != "undefined" {
			t.Fatalf("expected undefined, got %s", result)
		}
		r2.teardownStore()
	})

	t.Run("store holds complex values", func(t *testing.T) {
		a, _ := testAgent(t)

		r1 := newRun(a, "r1", "", "c1", context.Background())
		_, err := executeJS(r1.vmRuntime(), `store.items = [{name: "a", val: 1}, {name: "b", val: 2}]`)
		if err != nil {
			t.Fatal(err)
		}
		r1.teardownStore()

		r2 := newRun(a, "r2", "", "c1", context.Background())
		result, err := executeJS(r2.vmRuntime(), `store.items.length`)
		if err != nil {
			t.Fatal(err)
		}
		if result != "2" {
			t.Fatalf("expected 2, got %s", result)
		}
		result, err = executeJS(r2.vmRuntime(), `store.items[1].name`)
		if err != nil {
			t.Fatal(err)
		}
		if result != "b" {
			t.Fatalf("expected b, got %s", result)
		}
		r2.teardownStore()
	})

	t.Run("let const do not survive", func(t *testing.T) {
		a, _ := testAgent(t)

		r1 := newRun(a, "r1", "", "c1", context.Background())
		_, err := executeJS(r1.vmRuntime(), `var x = 99`)
		if err != nil {
			t.Fatal(err)
		}
		r1.teardownStore()

		r2 := newRun(a, "r2", "", "c1", context.Background())
		result, err := executeJS(r2.vmRuntime(), `typeof x`)
		if err != nil {
			t.Fatal(err)
		}
		if result != "undefined" {
			t.Fatalf("expected undefined, got %s", result)
		}
		r2.teardownStore()
	})

	t.Run("pure function survives", func(t *testing.T) {
		a, _ := testAgent(t)

		r1 := newRun(a, "r1", "", "c1", context.Background())
		_, err := executeJS(r1.vmRuntime(), `store.double = function(n) { return n * 2 }`)
		if err != nil {
			t.Fatal(err)
		}
		r1.teardownStore()

		r2 := newRun(a, "r2", "", "c1", context.Background())
		result, err := executeJS(r2.vmRuntime(), `store.double(21)`)
		if err != nil {
			t.Fatal(err)
		}
		if result != "42" {
			t.Fatalf("expected 42, got %s", result)
		}
		r2.teardownStore()
	})

	t.Run("closure does not survive", func(t *testing.T) {
		a, _ := testAgent(t)

		r1 := newRun(a, "r1", "", "c1", context.Background())
		_, err := executeJS(r1.vmRuntime(), `var base = 10; store.add = function(n) { return base + n }`)
		if err != nil {
			t.Fatal(err)
		}
		r1.teardownStore()

		// In the new VM, `base` is not defined. The function was re-evaluated
		// via eval(), so `base` references the new (empty) global scope.
		// Calling the function throws ReferenceError.
		r2 := newRun(a, "r2", "", "c1", context.Background())
		_, err = executeJS(r2.vmRuntime(), `store.add(5)`)
		if err == nil {
			t.Fatal("expected error for closure with dead scope, got nil")
		}
		r2.teardownStore()
	})

	t.Run("size tracking", func(t *testing.T) {
		a, _ := testAgent(t)

		r1 := newRun(a, "r1", "", "c1", context.Background())
		_, err := executeJS(r1.vmRuntime(), `store.data = "x".repeat(1000)`)
		if err != nil {
			t.Fatal(err)
		}
		r1.teardownStore()

		cvm := r1.convVM
		cvm.mu.Lock()
		size := cvm.storeSize
		cvm.mu.Unlock()
		if size < 1000 {
			t.Fatalf("expected storeSize >= 1000, got %d", size)
		}
	})

	t.Run("over threshold hard resets", func(t *testing.T) {
		a, _ := testAgent(t)
		a.convVMConfig.MaxStoreSize = 100 // very low threshold

		// Run 1: exceed threshold → needsCleanup = true
		r1 := newRun(a, "r1", "", "c1", context.Background())
		_, err := executeJS(r1.vmRuntime(), `store.big = "x".repeat(200)`)
		if err != nil {
			t.Fatal(err)
		}
		r1.teardownStore()
		if !r1.convVM.needsCleanup {
			t.Fatal("expected needsCleanup after first run")
		}

		// Run 2: still over threshold → hard reset
		r2 := newRun(a, "r2", "", "c1", context.Background())
		_, err = executeJS(r2.vmRuntime(), `store.big = "x".repeat(200)`)
		if err != nil {
			t.Fatal(err)
		}
		r2.teardownStore()

		r2.convVM.mu.Lock()
		storeLen := len(r2.convVM.store)
		r2.convVM.mu.Unlock()
		if storeLen != 0 {
			t.Fatalf("expected empty store after hard reset, got %d keys", storeLen)
		}
	})

	t.Run("idle gc", func(t *testing.T) {
		a, _ := testAgent(t)

		// Create a ConversationVM.
		r1 := newRun(a, "r1", "", "c1", context.Background())
		executeJS(r1.vmRuntime(), `store.x = 1`)
		r1.teardownStore()

		// Backdate lastUsed.
		cvm := r1.convVM
		cvm.mu.Lock()
		cvm.lastUsed = time.Now().Add(-4 * 24 * time.Hour) // 4 days ago
		cvm.mu.Unlock()

		// Manually trigger GC sweep.
		cutoff := time.Now().Add(-a.convVMConfig.IdleTimeout)
		a.conversationVMs.Range(func(key, value any) bool {
			cv := value.(*ConversationVM)
			cv.mu.Lock()
			idle := cv.lastUsed.Before(cutoff)
			cv.mu.Unlock()
			if idle {
				a.conversationVMs.Delete(key)
			}
			return true
		})

		// Verify c1 was swept — next run gets empty store.
		r2 := newRun(a, "r2", "", "c1", context.Background())
		result, err := executeJS(r2.vmRuntime(), `typeof store.x`)
		if err != nil {
			t.Fatal(err)
		}
		if result != "undefined" {
			t.Fatalf("expected undefined after GC, got %s", result)
		}
		r2.teardownStore()
	})

	t.Run("no store without conversation id", func(t *testing.T) {
		a, _ := testAgent(t)

		r := newRun(a, "r1", "", "", context.Background())
		result, err := executeJS(r.vmRuntime(), `typeof store`)
		if err != nil {
			t.Fatal(err)
		}
		if result != "undefined" {
			t.Fatalf("expected undefined, got %s", result)
		}
	})

	t.Run("store with nested mutations", func(t *testing.T) {
		a, _ := testAgent(t)

		r1 := newRun(a, "r1", "", "c1", context.Background())
		_, err := executeJS(r1.vmRuntime(), `store.config = {theme: "dark", fontSize: 14}`)
		if err != nil {
			t.Fatal(err)
		}
		r1.teardownStore()

		// Mutate nested value in second run.
		r2 := newRun(a, "r2", "", "c1", context.Background())
		_, err = executeJS(r2.vmRuntime(), `store.config.theme = "light"`)
		if err != nil {
			t.Fatal(err)
		}
		r2.teardownStore()

		// Verify mutation persisted.
		r3 := newRun(a, "r3", "", "c1", context.Background())
		result, err := executeJS(r3.vmRuntime(), `store.config.theme`)
		if err != nil {
			t.Fatal(err)
		}
		if result != "light" {
			t.Fatalf("expected light, got %s", result)
		}
		r3.teardownStore()
	})

	t.Run("store string values", func(t *testing.T) {
		a, _ := testAgent(t)

		r1 := newRun(a, "r1", "", "c1", context.Background())
		_, err := executeJS(r1.vmRuntime(), `store.name = "hello world"`)
		if err != nil {
			t.Fatal(err)
		}
		r1.teardownStore()

		r2 := newRun(a, "r2", "", "c1", context.Background())
		result, err := executeJS(r2.vmRuntime(), `store.name`)
		if err != nil {
			t.Fatal(err)
		}
		if result != "hello world" {
			t.Fatalf("expected 'hello world', got %s", result)
		}
		r2.teardownStore()
	})

	t.Run("store delete key", func(t *testing.T) {
		a, _ := testAgent(t)

		r1 := newRun(a, "r1", "", "c1", context.Background())
		_, err := executeJS(r1.vmRuntime(), `store.x = 1; store.y = 2`)
		if err != nil {
			t.Fatal(err)
		}
		r1.teardownStore()

		// Delete x in second run.
		r2 := newRun(a, "r2", "", "c1", context.Background())
		_, err = executeJS(r2.vmRuntime(), `delete store.x`)
		if err != nil {
			t.Fatal(err)
		}
		r2.teardownStore()

		// Verify x is gone but y survives.
		r3 := newRun(a, "r3", "", "c1", context.Background())
		result, _ := executeJS(r3.vmRuntime(), `typeof store.x`)
		if result != "undefined" {
			t.Fatalf("expected undefined, got %s", result)
		}
		result, _ = executeJS(r3.vmRuntime(), `store.y`)
		if result != "2" {
			t.Fatalf("expected 2, got %s", result)
		}
		r3.teardownStore()
	})
}

