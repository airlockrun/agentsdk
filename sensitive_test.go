package agentsdk

import (
	"testing"

	"github.com/airlockrun/goai/message"
)

func TestRedactSensitive(t *testing.T) {
	a, _ := testAgent(t)
	a.AddSensitive("secret123", "apikey456")

	result := a.redactSensitive("the secret123 is apikey456 ok")
	expected := "the [REDACTED] is [REDACTED] ok"
	if result != expected {
		t.Fatalf("expected %q, got %q", expected, result)
	}
}

func TestRedactMessages(t *testing.T) {
	a, _ := testAgent(t)
	a.AddSensitive("mysecret")

	msgs := []message.Message{
		message.NewUserMessage("please use mysecret to login"),
	}
	redacted := a.redactMessages(msgs)
	if redacted[0].Content.Text != "please use [REDACTED] to login" {
		t.Fatalf("expected redacted message, got %q", redacted[0].Content.Text)
	}
	// Original should be unchanged.
	if msgs[0].Content.Text != "please use mysecret to login" {
		t.Fatal("original message was modified")
	}
}
