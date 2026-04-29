package agentsdk

import (
	"strings"

	"github.com/airlockrun/goai/message"
)

// AddSensitive registers values that should be redacted from LLM messages.
func (a *Agent) AddSensitive(values ...string) {
	a.sensitiveM.Lock()
	defer a.sensitiveM.Unlock()
	for _, v := range values {
		if v != "" {
			a.sensitiveSet[v] = struct{}{}
		}
	}
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
