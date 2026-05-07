package agentsdk

import (
	"math"
	"strings"
	"unicode"

	"github.com/airlockrun/goai/message"
)

// AddSensitive registers values that should be redacted from LLM
// messages. Bypasses the IsLikelySecret filter — use only for values
// the framework knows are sensitive (e.g. the agent token); otherwise
// prefer maybeAddSensitive.
func (a *Agent) AddSensitive(values ...string) {
	a.sensitiveM.Lock()
	defer a.sensitiveM.Unlock()
	for _, v := range values {
		if v != "" {
			a.sensitiveSet[v] = struct{}{}
		}
	}
}

// maybeAddSensitive applies the IsLikelySecret heuristic before
// registering a value. Used by EnvVarHandle.Get for Secret=true vars to
// guard against an operator pasting a placeholder ("password",
// "changeme") and the redactor then nuking that word from every chat.
func (a *Agent) maybeAddSensitive(v string) {
	if !IsLikelySecret(v) {
		return
	}
	a.sensitiveM.Lock()
	a.sensitiveSet[v] = struct{}{}
	a.sensitiveM.Unlock()
}

// IsLikelySecret reports whether v looks like a real credential rather
// than a low-entropy placeholder or common phrase. Used to gate
// auto-redaction so values like "password", "true", or "hello world"
// don't end up in the redact set and nuke ordinary words from LLM input.
//
// Heuristic, calibrated empirically against real API-key shapes
// (sk-…, bb_live_…, AKIA…, ghp_…, JWTs, telegram bot tokens):
//
//   - Length ≥ 16 — every common credential format clears this; common
//     phrases ("password1234", "hello world!") don't.
//   - Reject all-digit strings and JSON literals regardless of length.
//   - Reject very-low-entropy strings (< 2.0 bits/char) to filter out
//     "aaaaaaaaaaaaaaaa"-style repeats. Real keys score 4–5 bits/char.
//
// This is a heuristic, not a security boundary. Operators who paste a
// real key are auto-protected; operators who paste a placeholder don't
// poison their own logs.
func IsLikelySecret(v string) bool {
	v = strings.TrimSpace(v)
	if len(v) < 16 {
		return false
	}
	switch strings.ToLower(v) {
	case "true", "false", "null", "undefined":
		return false
	}
	if isAllDigits(v) {
		return false
	}
	return shannonEntropy(v) >= 2.0
}

func isAllDigits(s string) bool {
	for _, r := range s {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// shannonEntropy returns the Shannon entropy (bits per char) of s.
// Empty string returns 0.
func shannonEntropy(s string) float64 {
	if s == "" {
		return 0
	}
	freq := make(map[rune]int, len(s))
	for _, r := range s {
		freq[r]++
	}
	n := float64(len(s))
	var h float64
	for _, c := range freq {
		p := float64(c) / n
		h -= p * math.Log2(p)
	}
	return h
}

// redactSensitive replaces all known sensitive values in s with [REDACTED].
// Internal helper used by redactMessages — builders never call this directly.
func (a *Agent) redactSensitive(s string) string {
	a.sensitiveM.RLock()
	defer a.sensitiveM.RUnlock()
	for v := range a.sensitiveSet {
		s = strings.ReplaceAll(s, v, "[REDACTED]")
	}
	return s
}

// redactMessages returns a copy of messages with sensitive values redacted from text content.
func (a *Agent) redactMessages(msgs []message.Message) []message.Message {
	a.sensitiveM.RLock()
	defer a.sensitiveM.RUnlock()
	if len(a.sensitiveSet) == 0 {
		return msgs
	}
	out := make([]message.Message, len(msgs))
	for i, m := range msgs {
		out[i] = m
		if m.Content.Text != "" {
			text := m.Content.Text
			for v := range a.sensitiveSet {
				text = strings.ReplaceAll(text, v, "[REDACTED]")
			}
			out[i].Content.Text = text
		}
	}
	return out
}
