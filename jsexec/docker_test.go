package jsexec

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDocker(t *testing.T) {
	if os.Getenv("JSEXEC_DOCKER_TEST") != "1" {
		t.Skip("set JSEXEC_DOCKER_TEST=1 for production image tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	const image = "jsexec-test:permission-isolation"
	if err := BuildImage(ctx, image); err != nil {
		t.Fatal(err)
	}
	options := Options{Limits: DefaultLimits(), Bindings: []Binding{{Name: "echo", Path: []string{"air", "echo"}}, {Name: "wait", Path: []string{"air", "wait"}}}}
	newSession := func(t *testing.T) *DockerSession {
		t.Helper()
		s, err := NewDockerSession(ctx, image, options)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	}
	echo := InvokerFunc(func(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
		return args, nil
	})
	run := func(t *testing.T, s Session, code string) Result {
		t.Helper()
		r, err := s.Execute(ctx, code, echo)
		if err != nil {
			t.Fatalf("execute: %v; result=%+v", err, r)
		}
		return r
	}
	t.Run("raw_attach", func(t *testing.T) {
		for _, name := range []string{"malformed", "overlap", "watchdog"} {
			t.Run(name, func(t *testing.T) {
				short, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				s, err := NewDockerSession(short, image, options)
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				if name == "malformed" {
					var header [4]byte
					binary.BigEndian.PutUint32(header[:], maxFrameBytes+1)
					if _, err := s.f.rw.Write(header[:]); err != nil {
						t.Fatal(err)
					}
				} else {
					o := options
					if name == "watchdog" {
						o.Limits.ExecutionMS = 100
					}
					m := frame{Type: "execute", ID: "00000000000000000000000000000001", Code: `while(true){}`, Options: &o}
					if err := s.f.write(m); err != nil {
						t.Fatal(err)
					}
					if name == "overlap" {
						m.ID = "00000000000000000000000000000002"
						if err := s.f.write(m); err != nil {
							t.Fatal(err)
						}
					}
				}
				m, err := s.f.read()
				if short.Err() != nil {
					t.Fatal("supervisor depended on client cancellation")
				}
				if err == nil && m.Type != "fatal" {
					t.Fatalf("invalid attach remained open: %+v", m)
				}
			})
		}
	})
	t.Run("state_logs_exception", func(t *testing.T) {
		s := newSession(t)
		r := run(t, s, `globalThis.state = {n: await Promise.resolve(41)}; console.log("hello", state.n); return await air.echo(state.n);`)
		if string(r.Output) != "[41]" || len(r.Logs) != 1 || r.Logs[0].Message != "hello 41" {
			t.Fatalf("result=%+v", r)
		}
		r = run(t, s, `return ++globalThis.state.n;`)
		if string(r.Output) != "42" {
			t.Fatal(string(r.Output))
		}
		_, err := s.Execute(ctx, `throw new TypeError("expected");`, echo)
		var js *Exception
		if !errors.As(err, &js) || js.Name != "TypeError" {
			t.Fatal(err)
		}
		r = run(t, s, `return state.n;`)
		if string(r.Output) != "42" {
			t.Fatal(string(r.Output))
		}
	})
	t.Run("local_log_and_user", func(t *testing.T) {
		for _, identity := range []*User{nil, {ID: "caller-123", Email: "alice@example.com", DisplayName: "Alice"}} {
			name := "anonymous"
			if identity != nil {
				name = "identified"
			}
			t.Run(name, func(t *testing.T) {
				o := Options{Limits: DefaultLimits(), User: identity}
				s, err := NewDockerSession(ctx, image, o)
				if err != nil {
					t.Fatal(err)
				}
				defer s.Close()
				callback := InvokerFunc(func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
					t.Error("local intrinsic called the broker")
					return nil, errors.New("unexpected RPC")
				})
				r, err := s.Execute(ctx, `air.log("hello", {n: 42}); console.warn("warning"); globalThis.savedLog = air.log; globalThis.user = {id: "forged"}; if (user !== null) { try { user.id = "forged"; } catch {} if (!Object.isFrozen(user)) throw Error("mutable caller"); } return {user, alias: air.log === console.log};`, callback)
				if err != nil || len(r.Logs) != 2 || r.Logs[0].Level != LogInfo || r.Logs[0].Message != `hello {"n":42}` || r.Logs[1].Level != LogWarn {
					t.Fatalf("result=%+v err=%v", r, err)
				}
				want, _ := json.Marshal(struct {
					User  *User `json:"user"`
					Alias bool  `json:"alias"`
				}{identity, true})
				if string(r.Output) != string(want) {
					t.Fatalf("output=%s want=%s", r.Output, want)
				}
				r, err = s.Execute(ctx, `savedLog("stale"); air.log("current"); return user;`, callback)
				want, _ = json.Marshal(identity)
				if err != nil || string(r.Output) != string(want) || len(r.Logs) != 1 || r.Logs[0].Message != "current" {
					t.Fatalf("retained scope result=%+v err=%v", r, err)
				}
			})
		}
	})
	t.Run("local_log_limit", func(t *testing.T) {
		o := Options{Limits: DefaultLimits()}
		o.Limits.Logs = 1
		s, err := NewDockerSession(ctx, image, o)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		r, err := s.Execute(ctx, `air.log("first"); air.log("overflow");`, echo)
		var js *Exception
		if !errors.As(err, &js) || js.Name != "RangeError" || len(r.Logs) != 1 {
			t.Fatalf("result=%+v err=%v", r, err)
		}
		if _, err := s.Execute(ctx, `return 1`, echo); !errors.Is(err, ErrClosed) {
			t.Fatalf("log overflow did not close session: %v", err)
		}
	})
	t.Run("parallel_and_drain", func(t *testing.T) {
		s := newSession(t)
		var active, maximum, finished atomic.Int32
		callback := InvokerFunc(func(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
			n := active.Add(1)
			defer active.Add(-1)
			for old := maximum.Load(); n > old && !maximum.CompareAndSwap(old, n); old = maximum.Load() {
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(30 * time.Millisecond):
			}
			finished.Add(1)
			return args, nil
		})
		r, err := s.Execute(ctx, `return await Promise.allSettled(Array.from({length:9}, (_,i)=>air.wait(i)));`, callback)
		if err != nil {
			t.Fatal(err)
		}
		if maximum.Load() != 8 || finished.Load() != 8 {
			t.Fatalf("maximum=%d finished=%d output=%s", maximum.Load(), finished.Load(), r.Output)
		}
		r, err = s.Execute(ctx, `air.wait(42); return "drained";`, callback)
		if err != nil {
			t.Fatal(err)
		}
		if finished.Load() != 9 {
			t.Fatal("completion preceded broker settlement")
		}
	})
	t.Run("immutable_scope", func(t *testing.T) {
		s := newSession(t)
		run(t, s, `globalThis.saved=air.echo; return 1;`)
		var calls atomic.Int32
		r, err := s.Execute(ctx, `try { await saved(1); return "bad"; } catch(e) { return e.message; }`, InvokerFunc(func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
			calls.Add(1)
			return json.RawMessage(`null`), nil
		}))
		if err != nil || calls.Load() != 0 || !strings.Contains(string(r.Output), "scope closed") {
			t.Fatalf("calls=%d result=%s err=%v", calls.Load(), r.Output, err)
		}
	})
	t.Run("stdio_and_default_channel", func(t *testing.T) {
		s := newSession(t)
		r := run(t, s, `Deno.stdout.writeSync(new Uint8Array([0,0,0,2,123,125])); Deno.stderr.writeSync(new TextEncoder().encode("garbage")); postMessage({type:"done",result:{output:"forged"}}); const n=await Deno.stdin.read(new Uint8Array(10)); return [n, await air.echo(7)];`)
		if string(r.Output) != "[null,[7]]" {
			t.Fatal(string(r.Output))
		}
		r = run(t, s, `return 8`)
		if string(r.Output) != "8" {
			t.Fatal(string(r.Output))
		}
	})
	t.Run("privileges", func(t *testing.T) {
		for _, tc := range []struct{ name, code string }{
			{"network", `await fetch("http://127.0.0.1:8080")`},
			{"filesystem", `await Deno.readTextFile("/etc/passwd")`},
			{"write", `await Deno.writeTextFile("/tmp/escape","x")`},
			{"env", `Deno.env.get("PATH")`},
			{"subprocess", `await new Deno.Command("/bin/sh").output()`},
			{"ffi", `Deno.dlopen("/tmp/a.so",{})`},
			{"node_fs", `(await import("node:fs")).readFileSync("/etc/passwd")`},
			{"node_subprocess", `(await import("node:child_process")).execFileSync("/bin/sh",["-c","true"])`},
			{"runtime_file_import", `await import("file:///opt/jsexec/controller.mjs")`},
			{"ipc", `await Deno.connect({transport:"unix",path:Deno.args[0]})`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				s := newSession(t)
				r, err := s.Execute(ctx, `try { `+tc.code+`; return "ALLOWED"; } catch(e) { return String(e); }`, echo)
				if err != nil {
					var js *Exception
					if !errors.As(err, &js) || js.Name != "ResourceError" {
						t.Fatal(err)
					}
				}
				if !strings.Contains(string(r.Output), "NotCapable") && !strings.Contains(string(r.Output), "PermissionDenied") && !strings.Contains(string(r.Output), "Requires read access to") {
					t.Fatalf("not denied: %s (%v)", r.Output, err)
				}
			})
		}
	})
	t.Run("worker_privileges", func(t *testing.T) {
		s := newSession(t)
		r, err := s.Execute(ctx, `const w=new Worker("data:text/javascript,"+encodeURIComponent('try { await fetch("http://127.0.0.1:8080"); postMessage("ALLOWED"); } catch(e) { postMessage(String(e)); }'),{type:"module",deno:{permissions:"inherit"}}); return await new Promise(resolve=>w.onmessage=e=>{w.terminate();resolve(e.data)});`, echo)
		var js *Exception
		if !errors.As(err, &js) || js.Name != "ResourceError" || !strings.Contains(string(r.Output), "NotCapable") {
			t.Fatalf("worker output=%s err=%v", r.Output, err)
		}
	})
	t.Run("worker_permission_escalation", func(t *testing.T) {
		s := newSession(t)
		r, err := s.Execute(ctx, `const w=new Worker("data:text/javascript,postMessage('ALLOWED')",{type:"module",deno:{permissions:{read:true}}}); return await new Promise(resolve=>{w.onmessage=e=>resolve(e.data);w.onerror=e=>{e.preventDefault();resolve(e.message)}});`, echo)
		text := string(r.Output)
		if err != nil {
			text += " " + err.Error()
		}
		if strings.Contains(text, "ALLOWED") || !strings.Contains(strings.ToLower(text), "escalat") {
			t.Fatalf("worker escalation not explicitly denied: %s", text)
		}
	})
	t.Run("callback_error_and_timer_cleanup", func(t *testing.T) {
		s := newSession(t)
		r, err := s.Execute(ctx, `await new Promise(resolve=>setTimeout(resolve,1)); try { await air.echo(1); return "bad"; } catch(e) { return e.message; }`, InvokerFunc(func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
			return nil, errors.New("denied by host")
		}))
		if err != nil || string(r.Output) != `"denied by host"` {
			t.Fatalf("%s %v", r.Output, err)
		}
		r = run(t, s, `return 42`)
		if string(r.Output) != "42" {
			t.Fatal(string(r.Output))
		}
	})
	t.Run("host_capability_validation", func(t *testing.T) {
		s := newSession(t)
		var calls atomic.Int32
		_, err := s.Execute(ctx, `return await invoke("not_allowed",1)`, InvokerFunc(func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
			calls.Add(1)
			return json.RawMessage(`null`), nil
		}))
		if err == nil || calls.Load() != 0 {
			t.Fatalf("err=%v calls=%d", err, calls.Load())
		}
	})
	t.Run("background_terminal", func(t *testing.T) {
		for _, tc := range []struct{ name, code string }{
			{"timer", `setTimeout(()=>air.echo(9),10000); return 1`},
			{"interval", `setInterval(()=>{},10000); return 1`},
			{"node_unref", `(await import("node:timers")).setTimeout(()=>{},10000).unref(); return 1`},
			{"node_promise_unref", `(await import("node:timers/promises")).setTimeout(10000,1,{ref:false}); return 1`},
			{"atomic_wait", `Atomics.waitAsync(new Int32Array(new SharedArrayBuffer(4)),0,0,10000); return 1`},
			{"native_accounting_tamper", `Deno[Deno.internal].core.setLeakTracingEnabled(false); (await import("node:timers/promises")).setTimeout(10000,1,{ref:false}); return 1`},
			{"worker", `new Worker("data:text/javascript,setInterval(()=>{},10000)",{type:"module"}); return 1`},
			{"node_worker", `const {Worker}=await import("node:worker_threads"); new Worker(new URL("data:text/javascript,setInterval(()=>{},10000)"),{type:"module"}); return 1`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				s := newSession(t)
				_, err := s.Execute(ctx, tc.code, echo)
				var js *Exception
				if !errors.As(err, &js) || js.Name != "ResourceError" {
					t.Fatalf("expected resource termination, got %v", err)
				}
				_, err = s.Execute(ctx, `return 2`, echo)
				if !errors.Is(err, ErrClosed) {
					t.Fatalf("session reusable: %v", err)
				}
			})
		}
	})
	t.Run("bootstrap_tamper", func(t *testing.T) {
		s := newSession(t)
		r := run(t, s, `JSON.stringify=()=>"forged"; Map.prototype.get=()=>null; MessagePort.prototype.postMessage=()=>{}; globalThis.onmessage=e=>postMessage(e.data); globalThis.invoke=()=>"wrong"; return await air.echo(42);`)
		if string(r.Output) != "[42]" {
			t.Fatal(string(r.Output))
		}
		r = run(t, s, `return await air.echo(43)`)
		if string(r.Output) != "[43]" {
			t.Fatal(string(r.Output))
		}
	})
	t.Run("prototype_scope_tamper", func(t *testing.T) {
		s := newSession(t)
		r := run(t, s, `Object.defineProperty(Object.prototype,"drained",{get(){globalThis.leaked=this;return ()=>{};}}); Object.defineProperty(Object.prototype,"error",{get(){globalThis.leaked=this;return "forged";}}); globalThis.saved=air.echo; await air.echo(1); return typeof globalThis.leaked;`)
		if string(r.Output) != `"undefined"` {
			t.Fatal(string(r.Output))
		}
		r = run(t, s, `try { await saved(2); return "bad"; } catch(e) { return e.message; }`)
		if !strings.Contains(string(r.Output), "scope closed") {
			t.Fatal(string(r.Output))
		}
	})
	t.Run("prototype_binding_capture", func(t *testing.T) {
		s := newSession(t)
		run(t, s, `Object.defineProperty(Array.prototype,"3",{set(value){Object.defineProperty(this,"3",{value,writable:true,enumerable:true,configurable:true}); if(value && typeof value.echo === "function") value.echo("forged");},configurable:true}); return 1;`)
		var calls atomic.Int32
		r, err := s.Execute(ctx, `return 2;`, InvokerFunc(func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
			calls.Add(1)
			return json.RawMessage(`null`), nil
		}))
		if err != nil || string(r.Output) != "2" || calls.Load() != 0 {
			t.Fatalf("retained prototype acquired new callback: calls=%d output=%s err=%v", calls.Load(), r.Output, err)
		}
	})
	t.Run("prototype_code_replacement", func(t *testing.T) {
		s := newSession(t)
		run(t, s, `const iterator=Array.prototype[Symbol.iterator]; Array.prototype[Symbol.iterator]=function(){if(this[0]==="invoke" && this[1]==="console") this[this.length-1]='await air.echo("forged"); return 2;'; return iterator.call(this);}; return 1;`)
		var calls atomic.Int32
		r, err := s.Execute(ctx, `return 2;`, InvokerFunc(func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
			calls.Add(1)
			return json.RawMessage(`null`), nil
		}))
		if err != nil || string(r.Output) != "2" || calls.Load() != 0 {
			t.Fatalf("retained iterator replaced code: calls=%d output=%s err=%v", calls.Load(), r.Output, err)
		}
	})
	t.Run("serialization_background_resource", func(t *testing.T) {
		s := newSession(t)
		_, err := s.Execute(ctx, `Array.prototype.toJSON=function(){setTimeout(()=>{globalThis.late=true},10000);return this;}; return 1;`, echo)
		var js *Exception
		if !errors.As(err, &js) || js.Name != "ResourceError" {
			t.Fatalf("completion serialization escaped resource accounting: %v", err)
		}
		if _, err := s.Execute(ctx, `return 2;`, echo); !errors.Is(err, ErrClosed) {
			t.Fatalf("background session reusable: %v", err)
		}
	})
	t.Run("limits", func(t *testing.T) {
		for _, tc := range []struct{ name, code string }{
			{"output", `return "x".repeat(300000)`},
			{"logs", `for(let i=0;i<300;i++) console.log("x"); return 1`},
			{"arguments", `return await air.echo("x".repeat(300000))`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				s := newSession(t)
				_, err := s.Execute(ctx, tc.code, echo)
				var js *Exception
				if !errors.As(err, &js) {
					t.Fatalf("limit not surfaced as JS error: %v", err)
				}
			})
		}
	})
	t.Run("cpu_cgroup_worker_cancellation", func(t *testing.T) {
		s := newSession(t)
		run(t, s, `return 1`)
		short, cancel := context.WithCancel(ctx)
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := s.Execute(short, `for(let i=0;i<4;i++) new Worker("data:text/javascript,while(true){}",{type:"module"}); await new Promise(()=>{});`, echo)
			done <- err
		}()
		time.Sleep(1200 * time.Millisecond)
		out, err := exec.CommandContext(ctx, "docker", "exec", s.ContainerID, "/bin/cat", "/sys/fs/cgroup/cpu.stat").CombinedOutput()
		if err != nil {
			t.Fatalf("cgroup stats: %v %s", err, out)
		}
		fields := strings.Fields(string(out))
		throttled := false
		for i := 0; i+1 < len(fields); i += 2 {
			if fields[i] == "nr_throttled" && fields[i+1] != "0" {
				throttled = true
			}
		}
		if !throttled {
			t.Fatalf("CPU quota did not throttle workers: %s", out)
		}
		t.Logf("worker CPU cgroup stats: %s", out)
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("worker cancellation hung")
		}
	})
	t.Run("cancel_cpu_and_busy", func(t *testing.T) {
		s := newSession(t)
		run(t, s, `return 1`)
		short, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { _, err := s.Execute(short, `while(true){}`, echo); done <- err }()
		time.Sleep(100 * time.Millisecond)
		_, err := s.Execute(ctx, `return 1`, echo)
		if !errors.Is(err, ErrBusy) {
			t.Fatal(err)
		}
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("cancellation hung")
		}
	})
	t.Run("memory_and_container_limits", func(t *testing.T) {
		s := newSession(t)
		run(t, s, `return 1`)
		out, err := exec.CommandContext(ctx, "docker", "inspect", "--format", `{{.HostConfig.Memory}} {{.HostConfig.MemorySwap}} {{.HostConfig.NanoCpus}} {{.HostConfig.PidsLimit}} {{.HostConfig.NetworkMode}} {{.HostConfig.ReadonlyRootfs}}`, s.ContainerID).CombinedOutput()
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(string(out)) != "268435456 268435456 1000000000 128 none true" {
			t.Fatal(string(out))
		}
		_, err = s.Execute(ctx, `globalThis.buffers=[]; while(true){const b=new Uint8Array(16*1024*1024);b.fill(1);buffers.push(b);}`, echo)
		if err == nil {
			t.Fatal("unbounded allocation succeeded")
		}
		t.Logf("allocation terminated: %v", err)
		_, err = s.Execute(ctx, `return 2`, echo)
		if !errors.Is(err, ErrClosed) {
			t.Fatal(err)
		}
	})
}
