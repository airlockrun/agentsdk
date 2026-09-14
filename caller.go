package agentsdk

import (
	"context"

	"github.com/airlockrun/agentsdk/internal/testcaller"
	"github.com/airlockrun/agentsdk/wire"
)

// CallerKind identifies the actor, independently of app access.
type CallerKind string

const (
	CallerAnonymous   CallerKind = "anonymous"
	CallerUser        CallerKind = "user"
	CallerApplication CallerKind = "application"
)

// Interface identifies the initiating surface. Its zero value is invalid.
type Interface string

const (
	InterfaceHTTP        Interface = "http"
	InterfaceChat        Interface = "chat"
	InterfaceMCP         Interface = "mcp"
	InterfaceSchedule    Interface = "schedule"
	InterfaceApplication Interface = "application"
	InterfaceUnknown     Interface = "unknown"
)

// ExecutionKind identifies the current execution mode. Its zero value is invalid.
type ExecutionKind string

const (
	ExecutionRequest    ExecutionKind = "request"
	ExecutionJob        ExecutionKind = "job"
	ExecutionBackground ExecutionKind = "background"
	ExecutionStartup    ExecutionKind = "startup"
	ExecutionUnknown    ExecutionKind = "unknown"
)

// User is a snapshot of human identity and display metadata, not credentials.
type User struct {
	ID          string
	Email       string
	DisplayName string
	// PlatformMember is true for every admitted platform human, including those
	// with public app access. It is not inferred from Access.
	PlatformMember bool
}

// Origin describes the initiating surface and the current execution mode.
type Origin struct {
	Interface Interface
	Platform  string
	ClientID  string
	Execution ExecutionKind
}

// Caller is an immutable snapshot of host-attributed execution metadata.
// Its zero value is invalid. User and Origin return independent value snapshots.
type Caller struct {
	kind         CallerKind
	access       Access
	user         User
	hasUser      bool
	initiator    User
	hasInitiator bool
	origin       Origin
}

func (c Caller) requireValid() {
	if c.kind == "" {
		panic("agentsdk: invalid zero Caller")
	}
}

// Kind returns the actor kind.
func (c Caller) Kind() CallerKind {
	c.requireValid()
	return c.kind
}

// Access returns the actor's resolved app access, not platform membership.
func (c Caller) Access() Access {
	c.requireValid()
	return c.access
}

// User returns the acting human, if present. Application and anonymous callers
// have no acting human; an app owner's identity is never substituted.
func (c Caller) User() (User, bool) {
	c.requireValid()
	return c.user, c.hasUser
}

// Initiator returns the known human who initiated the execution, if present.
// User executions retain the same human as User, including durable user jobs.
func (c Caller) Initiator() (User, bool) {
	c.requireValid()
	return c.initiator, c.hasInitiator
}

// Origin returns host-attributed source metadata, not authentication authority.
func (c Caller) Origin() Origin {
	c.requireValid()
	return c.origin
}

// CallerFromContext returns explicit framework or agenttest caller metadata.
// It never materializes a run or performs I/O. Nil and unbound contexts panic.
func CallerFromContext(ctx context.Context) Caller {
	var c Caller
	if r := runFromContext(ctx); r != nil {
		c = r.caller
	} else if l := lazyRunFromContext(ctx); l != nil {
		c = l.caller
	} else if test, ok := testcaller.FromContext(ctx); ok {
		c = callerFromWire(test)
	}
	c.requireValid()
	return c
}

func callerFromWire(w wire.Caller) Caller {
	if err := w.Validate(); err != nil {
		panic("agentsdk: invalid caller: " + err.Error())
	}
	c := Caller{kind: CallerKind(w.Kind), access: Access(w.Access), origin: Origin{
		Interface: Interface(w.Origin.Interface), Platform: w.Origin.Platform,
		ClientID: w.Origin.ClientID, Execution: ExecutionKind(w.Origin.Execution),
	}}
	if w.User != nil {
		c.user, c.hasUser = User(*w.User), true
	}
	if w.Initiator != nil {
		c.initiator, c.hasInitiator = User(*w.Initiator), true
	}
	return c
}
