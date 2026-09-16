package agentruntime

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/airlockrun/goai/stream"
	"github.com/airlockrun/goai/tool"
)

func TestToolsUseExactChildAndCompletionSchemas(t *testing.T) {
	in, _ := input()
	addChild(&in)
	in.Definition.OutputSchema = json.RawMessage(`{"$defs":{"Node":{"type":"object","properties":{"n":{"type":"integer","minimum":9007199254740993},"next":{"anyOf":[{"$ref":"#/$defs/Node"},{"type":"null"}]},"ref":{"const":{"$ref":"#"}}},"required":["n"]}},"$ref":"#/$defs/Node"}`)
	set, spawns, err := tools(in, tool.New("run_js").Build())
	if err != nil {
		t.Fatal(err)
	}
	if string(set["spawn_research"].InputSchema) != string(in.Subagents[0].InputSchema) || spawns["spawn_research"] != "research" {
		t.Fatal("child schema was modified")
	}
	for _, tc := range []struct {
		name, value string
		valid       bool
	}{
		{"recursive typed output", `{"kind":"output","output":{"n":9007199254740993,"next":{"n":9007199254740994,"ref":{"$ref":"#"}}}}`, true},
		{"precision", `{"kind":"output","output":{"n":9007199254740992}}`, false},
		{"needs input", `{"kind":"needs_input","question":"Which project?"}`, true},
		{"wrong discriminator", `{"kind":"yield","question":"Which project?"}`, false},
		{"mixed output question", `{"kind":"output","output":{"n":9007199254740993},"question":""}`, false},
		{"empty question", `{"kind":"needs_input","question":""}`, false},
		{"untyped output", `{"kind":"output","output":42}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateJSON(set["complete"].InputSchema, json.RawMessage(tc.value)); (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v schema=%s", tc.valid, err, set["complete"].InputSchema)
			}
		})
	}
}

func TestRunEnforcesSDKArrayAndNumericConstraints(t *testing.T) {
	// agentTypeSchema retains the generated types at the root and adds Go array
	// lengths and numeric widths through allOf constraints with JSON numbers.
	contract := json.RawMessage(`{"type":"object","properties":{"values":{"type":"array","items":{"type":"integer"}},"count":{"type":"integer"}},"required":["values","count"],"allOf":[{"properties":{"values":{"minItems":2,"maxItems":2,"items":{"minimum":-128,"maximum":127}},"count":{"minimum":0,"maximum":18446744073709551615}}}]}`)
	for _, tc := range []struct {
		name, value string
		valid       bool
	}{
		{"exact bounds", `{"values":[-128,127],"count":18446744073709551615}`, true},
		{"short array", `{"values":[1],"count":0}`, false},
		{"long array", `{"values":[1,2,3],"count":0}`, false},
		{"signed underflow", `{"values":[-129,0],"count":0}`, false},
		{"signed overflow", `{"values":[0,128],"count":0}`, false},
		{"unsigned underflow", `{"values":[0,1],"count":-1}`, false},
		{"unsigned overflow", `{"values":[0,1],"count":18446744073709551616}`, false},
		{"wrong integer type", `{"values":[0,"1"],"count":0}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in, model := input(batch(
				call("spawn", "spawn_research", tc.value),
				call("output", "complete", `{"kind":"output","output":`+tc.value+`}`),
				call("question", "complete", `{"kind":"needs_input","question":"Which valid values should I use?"}`),
			))
			addChild(&in)
			in.Subagents[0].InputSchema, in.Definition.OutputSchema = contract, contract
			result, err := Run(t.Context(), in)
			if err != nil || result == nil || result.Reply == nil || len(model.DoStreamCalls) != 1 {
				t.Fatalf("result=%+v error=%v model calls=%d", result, err, len(model.DoStreamCalls))
			}
			control := in.Controller.(*controller)
			cp, _ := in.Store.LoadCheckpoint(t.Context())
			if tc.valid {
				if result.Reply.Kind != "output" || control.spawnCalls != 1 || control.completeCalls != 1 || cp.Calls[2].Result.Parts[0].Tool.Outcome != "denied" {
					t.Fatalf("result=%+v controller=%+v checkpoint=%+v", result, control, cp)
				}
			} else {
				if result.Reply.Kind != "needs_input" || control.spawnCalls != 0 || control.completeCalls != 1 {
					t.Fatalf("result=%+v controller=%+v", result, control)
				}
				for _, call := range cp.Calls[:2] {
					if call.Result.Parts[0].Tool.Outcome != "error" {
						t.Fatalf("invalid call=%+v", call)
					}
				}
			}
		})
	}
}

func TestControlValidationPreventsDispatch(t *testing.T) {
	for _, tc := range []struct{ name, args string }{
		{"spawn_research", `{"query":42}`},
		{"continue_agent", `{"sessionId":"","prompt":"more"}`},
		{"get_calls", `{"ids":[]}`},
		{"get_calls", `{"ids":["same","same"]}`},
		{"cancel_calls", `{"ids":[""]}`},
		{"wait_calls", `{"ids":["one"],"mode":"some"}`},
		{"wait_calls", `{"ids":["one"],"mode":"all","timeoutMs":-1}`},
		{"wait_calls", `{"ids":["one"],"mode":"all","timeoutMs":null}`},
		{"complete", `{"kind":"needs_input","question":"  "}`},
		{"complete", `{"kind":"output","output":{"answer":1},"unexpected":true}`},
		{"complete", `{"kind":"output","output":{"answer":1},"question":""}`},
		{"complete", `null`},
		{"run_js", `{"code":"return 1"}`},
	} {
		t.Run(tc.name+tc.args, func(t *testing.T) {
			in, _ := input(batch(call("invalid", tc.name, tc.args), complete("done")))
			addChild(&in)
			result, err := Run(t.Context(), in)
			if err != nil || result.Reply == nil {
				t.Fatalf("result=%+v error=%v", result, err)
			}
			cp, _ := in.Store.LoadCheckpoint(t.Context())
			if cp.Calls[0].Result.Parts[0].Tool.Outcome != "error" {
				t.Fatal("invalid call did not produce an error")
			}
			c := in.Controller.(*controller)
			if strings.Join(c.log, ",") != "model,complete" {
				t.Fatal(c.log)
			}
		})
	}
}

func TestControlValidationRejectsUnavailableToolBeforeDispatch(t *testing.T) {
	in, _ := input(batch(call("invalid", "unknown", `{}`), complete("done")))
	addChild(&in)
	result, err := Run(t.Context(), in)
	if result != nil || err == nil || !strings.Contains(err.Error(), "unavailable tool 'unknown'") {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	if strings.Join(in.Controller.(*controller).log, ",") != "model" {
		t.Fatal(in.Controller.(*controller).log)
	}
}

func TestRunRejectsMalformedModelCallIdentity(t *testing.T) {
	for _, tc := range []struct {
		name      string
		responses [][]stream.Event
	}{
		{"empty ID", [][]stream.Event{batch(complete(""))}},
		{"duplicate batch ID", [][]stream.Event{batch(call("same", "get_calls", `{"ids":["child"]}`), complete("same"))}},
		{"reused prior ID", [][]stream.Event{batch(call("same", "get_calls", `{"ids":["child"]}`)), batch(complete("same"))}},
		{"invalid JSON", [][]stream.Event{batch(call("invalid", "complete", `{"kind":`))}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in, _ := input(tc.responses...)
			if _, err := Run(t.Context(), in); err == nil {
				t.Fatal("invalid call accepted")
			}
			if in.Controller.(*controller).completeCalls != 0 {
				t.Fatal("invalid response dispatched")
			}
		})
	}
}
