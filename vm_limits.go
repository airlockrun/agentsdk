package agentsdk

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/dop251/goja"
)

// L3b tunables. Vars (not consts) so tests can shrink them; they are tuning
// knobs, not shared mutable state — never mutated at runtime.
//
// There is intentionally no heap/memory guard here: goja exposes no per-VM
// accounting and runtime.ReadMemStats() is process-wide, so any heap-based
// trip is unattributable across concurrent runs and would degrade innocent
// ones. Memory is bounded only by the exact, per-crossing mechanisms (L1
// per-call cap, L4 amplifier shims).
var (
	// jsMonitorInterval is how often the guard samples JS-CPU.
	jsMonitorInterval = 250 * time.Millisecond
	// jsCPULimit caps wall time attributable to JS execution (total minus
	// time parked in Go calls), so a long legit download is never charged
	// as CPU. Uses the go-call accounting below.
	jsCPULimit = 60 * time.Second
)

var errJSCPU = errors.New("run_js aborted: CPU time limit exceeded (no I/O progress — likely an unbounded loop)")

// goWall accumulates time the JS goroutine spends parked inside blocking Go
// calls (backend HTTP, external httpRequest), so the CPU guard can subtract
// it: a 10-minute download is not a 10-minute CPU spin. JS calls are
// sequential within one run (JS is single-threaded; a Go call blocks it), so
// at most one call is open at a time per run.
// Nesting-safe: a binding may wrap an inner client.do; only the outermost
// span is timed, so instrumentation is composable without double-counting.
type goWall struct {
	mu     sync.Mutex
	total  time.Duration
	depth  int
	openAt time.Time
}

func (g *goWall) enter() {
	g.mu.Lock()
	if g.depth == 0 {
		g.openAt = time.Now()
	}
	g.depth++
	g.mu.Unlock()
}

func (g *goWall) exit() {
	g.mu.Lock()
	if g.depth == 1 {
		g.total += time.Since(g.openAt)
		g.openAt = time.Time{}
	}
	if g.depth > 0 {
		g.depth--
	}
	g.mu.Unlock()
}

// elapsed returns total Go-call time including any currently-open span.
func (g *goWall) elapsed() time.Duration {
	g.mu.Lock()
	d := g.total
	if g.depth > 0 {
		d += time.Since(g.openAt)
	}
	g.mu.Unlock()
	return d
}

type goWallKey struct{}

// goWallFrom returns the run's go-call accumulator carried in ctx, or nil.
func goWallFrom(ctx context.Context) *goWall {
	gw, _ := ctx.Value(goWallKey{}).(*goWall)
	return gw
}

// withGoWall attaches gw to ctx so the central HTTP seam can credit blocking
// time without every binding being wrapped.
func withGoWall(ctx context.Context, gw *goWall) context.Context {
	return context.WithValue(ctx, goWallKey{}, gw)
}

// startJSGuard launches the L3b monitor for one run_js execution: it
// enforces the JS-attributable CPU budget (wall time minus time parked in
// Go calls, so a long legit download is never charged as a spin) and relays
// ctx cancellation (the existing prompt-timeout / disconnect behavior).
// There is deliberately no memory check — see the package note. The
// returned stop func must be called when execution finishes.
func startJSGuard(ctx context.Context, vm *goja.Runtime, gw *goWall) (stop func()) {
	done := make(chan struct{})
	startedAt := time.Now()
	// Go-call time already accumulated before this run_js call must not
	// count toward this call's JS budget.
	gwBase := gw.elapsed()

	go func() {
		t := time.NewTicker(jsMonitorInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				vm.Interrupt(ctx.Err())
				return
			case <-done:
				return
			case <-t.C:
				jsTime := time.Since(startedAt) - (gw.elapsed() - gwBase)
				if jsTime > jsCPULimit {
					vm.Interrupt(errJSCPU)
					return
				}
			}
		}
	}()

	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}

// L4 — amplifier-builtin guards.
//
// A handful of JS builtins turn a tiny input into one giant heap allocation
// in a single call: String.prototype.repeat / padStart / padEnd, the
// ArrayBuffer + TypedArray constructors, and Array.prototype.fill on a huge
// length. goja does not bound these (verified: stringproto_repeat does
// sb.Grow(len*count) with no max-length check), so `"x".repeat(1e10)` is a
// *non-recoverable* Go `fatal error: runtime: out of memory` that kills the
// whole agentsdk process — recover() can't catch it, and goja's Interrupt is
// never checked mid-builtin, so no watchdog can stop it either.
//
// We own the Runtime, so we shim each amplifier with a pre-check that throws
// a catchable RangeError before the allocation. This is a deliberate, finite
// denylist of the realistic amplifiers (not a general proof) — sufficient for
// the threat model here: runaway / mistaken LLM code, not an adversary
// hunting goja internals. The native implementations are captured in
// closures and never re-exposed, so user code cannot restore the uncapped
// originals (it can only delete the method, which removes the amplifier).
//
// Pairs with L1 (Go→JS boundary byte cap) and L3 (heap-growth monitor): L1
// caps bytes entering from Go tools, L4 caps bytes synthesized inside JS, L3
// catches gradual accumulation from any source.

// amplifierGuardJS is the bootstrap installed into every VM before user code.
// %d is maxJSValueBytes (the shared in-heap byte cap).
const amplifierGuardJS = `(function () {
  var CAP = %d;
  function tooBig(what) {
    throw new RangeError(what + ": result too large for run_js (exceeds " +
      (CAP >> 20) + " MiB in-memory cap) — build it incrementally or stream via storage instead");
  }

  var repeat = String.prototype.repeat;
  if (repeat) {
    String.prototype.repeat = function (count) {
      var n = Number(count);
      if (isFinite(n) && n >= 0 && this != null && String(this).length * n > CAP) {
        tooBig("String.prototype.repeat");
      }
      return repeat.call(this, count);
    };
  }

  ["padStart", "padEnd"].forEach(function (m) {
    var orig = String.prototype[m];
    if (!orig) return;
    String.prototype[m] = function (targetLength) {
      if (Number(targetLength) > CAP) tooBig("String.prototype." + m);
      return orig.apply(this, arguments);
    };
  });

  var fill = Array.prototype.fill;
  if (fill) {
    Array.prototype.fill = function () {
      // ~8 bytes per element is a deliberate lower-bound estimate; the goal
      // is to reject materializing a multi-GB backing array, not to be exact.
      if (this != null && Number(this.length) * 8 > CAP) {
        tooBig("Array.prototype.fill");
      }
      return fill.apply(this, arguments);
    };
  }

  // A function wrapper (not a Proxy): goja's instanceof rejects a Proxy as
  // the right-hand side, so "x instanceof Uint8Array" would break. A plain
  // function whose .prototype is the native prototype keeps instanceof,
  // .constructor, and static methods (via setPrototypeOf) intact.
  function capCtor(name, bytesPerUnit) {
    var C = this[name];
    if (typeof C !== "function") return;
    function W() {
      var first = arguments[0];
      if (typeof first === "number" && first * bytesPerUnit > CAP) {
        tooBig(name);
      }
      return Reflect.construct(C, arguments, W);
    }
    W.prototype = C.prototype;
    Object.setPrototypeOf(W, C); // inherit static methods (from/of/etc.)
    try { Object.defineProperty(W, "name", { value: name }); } catch (e) {}
    this[name] = W;
  }
  var g = (typeof globalThis !== "undefined") ? globalThis : this;
  capCtor.call(g, "ArrayBuffer", 1);
  capCtor.call(g, "SharedArrayBuffer", 1);
  [
    ["Int8Array", 1], ["Uint8Array", 1], ["Uint8ClampedArray", 1],
    ["Int16Array", 2], ["Uint16Array", 2],
    ["Int32Array", 4], ["Uint32Array", 4],
    ["Float32Array", 4], ["Float64Array", 8],
    ["BigInt64Array", 8], ["BigUint64Array", 8],
  ].forEach(function (p) { capCtor.call(g, p[0], p[1]); });
})();`

// amplifierGuardProgram is compiled once; the bootstrap is static.
var amplifierGuardProgram = goja.MustCompile(
	"<amplifier-guards>",
	fmt.Sprintf(amplifierGuardJS, maxJSValueBytes),
	true,
)

// installAmplifierGuards runs the L4 bootstrap in vm. It must run before any
// user code so the native amplifiers are captured and replaced first.
func installAmplifierGuards(vm *goja.Runtime) {
	if _, err := vm.RunProgram(amplifierGuardProgram); err != nil {
		// Static, compile-tested bootstrap — a failure here is a programming
		// error, not a runtime condition. Fail loud.
		panic(fmt.Errorf("agentsdk: installing run_js amplifier guards: %w", err))
	}
}
