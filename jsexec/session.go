// Package jsexec defines the transport-neutral JavaScript execution contract.
// Permission isolation and resource limits are described in README.md.
package jsexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Session owns one uninterrupted JavaScript realm. Implementations must reject
// overlapping Execute calls, not queue them. Close is idempotent and cancels
// execution. Loss of the realm is terminal: never recreate it or replay code.
//
// Code is an async function body: await and an explicit return are supported.
// Function-local declarations do not persist. Properties stored on globalThis
// persist across executions for the uninterrupted lifetime of this session.
// This state is ephemeral, not durable or shared across replicas.
type Session interface {
	Execute(ctx context.Context, code string, callback Invoker) (Result, error)
	Close() error
}

// Invoker dispatches a named capability with a JSON array of positional args.
// The caller must supply a non-nil invoker even when no capabilities are exposed.
// Implementations must honor context cancellation and may be called concurrently
// up to the session's finite broker limit. Results must be valid JSON.
//
// Each Execute immutably binds its invoker and context. A retained JS function
// from a completed invocation must not gain access to a subsequent invoker.
// No callback may begin after Execute returns; all accepted broker requests must
// settle or be canceled before completion. Session implementations must enforce
// these rules outside the untrusted JavaScript realm.
type Invoker interface {
	Invoke(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error)
}

// Result carries a JSON return value and ordered, bounded console output.
// Output is absent only for JavaScript undefined; JSON null is the bytes "null".
// Logs may accompany an execution error. Implementations must enforce finite
// code, frame, output, log, execution-time, and concurrent broker limits.
type Result struct {
	Output    json.RawMessage `json:"output,omitempty"`
	Undefined bool            `json:"undefined,omitempty"`
	Logs      []Log           `json:"logs,omitempty"`
}

type Log struct {
	Level   LogLevel `json:"level"`
	Message string   `json:"message"`
}

type LogLevel string

const (
	LogInfo  LogLevel = "info"
	LogWarn  LogLevel = "warn"
	LogError LogLevel = "error"
)

// Exception is a JavaScript execution failure, not a transport failure.
type Exception struct {
	Name    string `json:"name"`
	Message string `json:"message"`
}

func (e *Exception) Error() string { return e.Name + ": " + e.Message }

var (
	ErrBusy   = errors.New("jsexec: invocation already executing")
	ErrClosed = errors.New("jsexec: session closed")
)

// InvokerFunc adapts a function to Invoker.
type InvokerFunc func(context.Context, string, json.RawMessage) (json.RawMessage, error)

func (f InvokerFunc) Invoke(ctx context.Context, name string, args json.RawMessage) (json.RawMessage, error) {
	return f(ctx, name, args)
}

// Binding exposes a capability under a JavaScript path, for example
// {Name: "file_read", Path: []string{"air", "fileRead"}}. Invoke receives Name.
// Authorization and argument validation remain the host Invoker's responsibility.
type Binding struct {
	Name string   `json:"name"`
	Path []string `json:"path"`
}

type Options struct {
	Limits   Limits    `json:"limits"`
	Bindings []Binding `json:"bindings"`
	// User is optional display context, never an authorization source. The host
	// supplies it from the authenticated caller. Nil exposes JavaScript null.
	User *User `json:"user,omitempty"`
}

// User is the only caller metadata sent into the credential-free realm.
// Do not add tokens, grants, or other authorization state here.
type User struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
}

// Limits are required. DefaultLimits supplies an explicit starting policy.
// Wire deadlines are milliseconds; all size limits count UTF-8 bytes.
type Limits struct {
	FrameBytes      int `json:"frameBytes"`
	CodeBytes       int `json:"codeBytes"`
	OutputBytes     int `json:"outputBytes"`
	LogBytes        int `json:"logBytes"`
	Logs            int `json:"logs"`
	ConcurrentCalls int `json:"concurrentCalls"`
	Calls           int `json:"calls"`
	ExecutionMS     int `json:"executionMS"`
	IdleMS          int `json:"idleMS"`
}

func DefaultLimits() Limits {
	return Limits{FrameBytes: 1 << 20, CodeBytes: 256 << 10, OutputBytes: 256 << 10, LogBytes: 64 << 10, Logs: 256, ConcurrentCalls: 8, Calls: 256, ExecutionMS: 10000, IdleMS: 60000}
}

func (o Options) validate() error {
	l := o.Limits
	if l.FrameBytes < 1024 || l.FrameBytes > maxFrameBytes || l.CodeBytes < 1 || l.CodeBytes > l.FrameBytes/2 || l.OutputBytes < 1 || l.OutputBytes > l.FrameBytes/2 || l.LogBytes < 1 || l.LogBytes > l.FrameBytes/4 || l.Logs < 1 || l.Logs > 1024 || l.ConcurrentCalls < 1 || l.ConcurrentCalls > 32 || l.Calls < l.ConcurrentCalls || l.Calls > 4096 || l.ExecutionMS < 1 || l.ExecutionMS > 600000 || l.IdleMS < 1 || l.IdleMS > 600000 {
		return errors.New("jsexec: invalid finite limits")
	}
	if len(o.Bindings) > 1024 {
		return errors.New("jsexec: too many bindings")
	}
	if o.User != nil && (o.User.ID == "" || len(o.User.ID) > 256 || len(o.User.Email) > 1024 || len(o.User.DisplayName) > 1024) {
		return errors.New("jsexec: user requires an ID and bounded display claims")
	}
	names, paths := map[string]bool{}, map[string]bool{".air.log": true}
	for _, b := range o.Bindings {
		if b.Name == "" || len(b.Name) > 256 || names[b.Name] || len(b.Path) < 1 || len(b.Path) > 8 {
			return errors.New("jsexec: invalid or duplicate binding")
		}
		names[b.Name] = true
		path := ""
		for i, p := range b.Path {
			if !identifier(p) || p == "__proto__" || p == "prototype" || p == "constructor" {
				return fmt.Errorf("jsexec: invalid binding path %q", p)
			}
			if i == 0 && strings.Contains(" invoke console user globalThis arguments eval await break case catch class const continue debugger default delete do else enum export extends false finally for function if implements import in instanceof interface let new null package private protected public return static super switch this throw true try typeof var void while with yield ", " "+p+" ") {
				return errors.New("jsexec: reserved binding root")
			}
			path += "." + p
			if paths[path] {
				return errors.New("jsexec: binding path collision")
			}
		}
		paths[path] = true
	}
	for p := range paths {
		for q := range paths {
			if len(q) > len(p) && q[:len(p)+1] == p+"." {
				return errors.New("jsexec: binding namespace collision")
			}
		}
	}
	return nil
}

func identifier(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for i, c := range []byte(s) {
		if !(c == '_' || c == '$' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || i > 0 && c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func (l Limits) executionTimeout() time.Duration {
	return time.Duration(l.ExecutionMS) * time.Millisecond
}
