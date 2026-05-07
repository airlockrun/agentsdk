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

func TestIsLikelySecret(t *testing.T) {
	cases := []struct {
		v    string
		want bool
	}{
		// Real-shaped credentials (random-looking, ≥16 chars) must pass.
		{"sk-proj-AbCdEf1234567890GhIjKlMnOpQrSt", true}, // openai-style
		{"bb_live_mBeGGVZIlQ5CdAHBKCqIgWSbk0Y", true},   // browserbase (real shape)
		{"AKIAIOSFODNN7EXAMPLE", true},                   // AWS access key id
		{"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1In0.signature", true}, // JWT-shaped
		{"123456:AAH-x7lCqK9aZmNbVcXyQwErTyUiOpAsDfGh", true},     // telegram bot
		{"ghp_AbC3xY7zKlMn2pQrStUvWxYz0123456789", true},          // github PAT

		// Common words / placeholders / phrases must NOT pass.
		{"", false},
		{"a", false},
		{"password", false},        // 8 chars
		{"password1234", false},    // 12 chars — under min length
		{"hello world!", false},    // 12 chars — under min length
		{"changeme", false},
		{"true", false},
		{"false", false},
		{"null", false},
		{"1234567890123456", false}, // 16 digits — isAllDigits filter
		{"                ", false}, // whitespace
		{"aaaaaaaaaaaaaaaaaaaa", false}, // 20 of 'a' — low entropy
	}

	for _, c := range cases {
		got := IsLikelySecret(c.v)
		if got != c.want {
			t.Errorf("IsLikelySecret(%q) = %v, want %v", c.v, got, c.want)
		}
	}
}

func TestMaybeAddSensitive(t *testing.T) {
	a, _ := testAgent(t)

	// Junk values shouldn't enter the set.
	a.maybeAddSensitive("password")
	a.maybeAddSensitive("a")
	a.maybeAddSensitive("")
	if got := a.redactSensitive("my password is a"); got != "my password is a" {
		t.Errorf("low-entropy values should not be redacted: got %q", got)
	}

	// A real-looking key does enter the set.
	a.maybeAddSensitive("sk-proj-AbCdEf1234567890GhIjKlMnOpQrSt")
	if got := a.redactSensitive("the key is sk-proj-AbCdEf1234567890GhIjKlMnOpQrSt here"); got != "the key is [REDACTED] here" {
		t.Errorf("real key should be redacted: got %q", got)
	}
}
