package agenttest

import (
	"flag"
	"io"
	"reflect"
	"testing"
)

func TestRegisterFlags(t *testing.T) {
	before := flag.CommandLine.Lookup("agenttest.model")
	fs := flag.NewFlagSet("agenttest", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	options := RegisterFlags(fs)
	if len(options.Models) != 0 {
		t.Fatalf("default options = %+v", options)
	}
	if flag.CommandLine.Lookup("agenttest.model") != before {
		t.Fatal("changed global FlagSet")
	}
	if err := fs.Parse([]string{"-agenttest.model=summary=work/openai/gpt-5.4", "-agenttest.model=@default=personal/anthropic/claude"}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(options.Models, map[string]string{"summary": "work/openai/gpt-5.4", "": "personal/anthropic/claude"}) {
		t.Fatalf("options = %+v", options)
	}
	if got := fs.Lookup("agenttest.model").Value.String(); got != "@default=personal/anthropic/claude,summary=work/openai/gpt-5.4" {
		t.Fatalf("flag string = %q", got)
	}
}

func TestModelSelectionFlagsRejectInvalidValues(t *testing.T) {
	for _, args := range [][]string{
		{"-agenttest.model=summary"},
		{"-agenttest.model==openai/gpt"},
		{"-agenttest.model=summary=gpt"},
		{"-agenttest.model=summary=/gpt"},
		{"-agenttest.model=summary=openai/"},
		{"-agenttest.model=summary=openai/gpt 5"},
		{"-agenttest.model=summary=openai/gpt", "-agenttest.model=summary=openai/other"},
		{"-agenttest.model=@default=openai/gpt", "-agenttest.model=@default=openai/other"},
		{"-agenttest.model=summary=work/openai/gpt", "-agenttest.model=summary=work/openai/other"},
		{"-agenttest.auth=codex"},
	} {
		t.Run(args[0], func(t *testing.T) {
			fs := flag.NewFlagSet("agenttest", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			RegisterFlags(fs)
			if err := fs.Parse(args); err == nil {
				t.Fatal("invalid flags accepted")
			}
		})
	}
}

func TestRegisterFlagsRequiresExplicitFlagSet(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("nil FlagSet did not panic")
		}
	}()
	RegisterFlags(nil)
}
