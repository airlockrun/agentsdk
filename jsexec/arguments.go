package jsexec

import (
	"context"
	"encoding/json"
)

// ArgumentSession executes a JS body with a structured lexical args object.
// Arguments travel over the existing framed callback channel, never as source.
// The intrinsic is invocation-bound and grants no host capability.
type ArgumentSession interface {
	Session
	ExecuteWithArgs(context.Context, string, json.RawMessage, Invoker) (Result, error)
}

const MaxArgsBytes = 64 << 10
const scriptArgsCapability = "jsexec/script_args"
const scriptArgsBinding = "__scriptArgs"

func validArguments(args json.RawMessage) bool {
	if len(args) == 0 || len(args) > MaxArgsBytes {
		return false
	}
	var value map[string]json.RawMessage
	return json.Unmarshal(args, &value) == nil && value != nil
}
