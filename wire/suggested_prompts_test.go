package wire

import (
	"strings"
	"testing"
)

func TestValidateSuggestedPrompts(t *testing.T) {
	for _, tt := range []struct {
		name    string
		prompts []SuggestedPrompt
		invalid bool
	}{
		{"absent", nil, false},
		{"valid", []SuggestedPrompt{{Title: "Найти товар", Prompt: "Помоги найти товар.\nЦена:\t"}}, false},
		{"limits", []SuggestedPrompt{{Title: strings.Repeat("x", MaxSuggestedPromptTitleBytes), Prompt: strings.Repeat("x", MaxSuggestedPromptBytes)}}, false},
		{"blank title", []SuggestedPrompt{{Title: " ", Prompt: "Find"}}, true},
		{"blank prompt", []SuggestedPrompt{{Title: "Find", Prompt: "\n "}}, true},
		{"long title", []SuggestedPrompt{{Title: strings.Repeat("x", MaxSuggestedPromptTitleBytes+1), Prompt: "Find"}}, true},
		{"long prompt", []SuggestedPrompt{{Title: "Find", Prompt: strings.Repeat("x", MaxSuggestedPromptBytes+1)}}, true},
		{"invalid utf8", []SuggestedPrompt{{Title: "Find", Prompt: "\xff"}}, true},
		{"control title", []SuggestedPrompt{{Title: "Find\n", Prompt: "Find"}}, true},
		{"control prompt", []SuggestedPrompt{{Title: "Find", Prompt: "Find\x00"}}, true},
		{"count", make([]SuggestedPrompt, MaxSuggestedPrompts+1), true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := ValidateSuggestedPrompts(tt.prompts); (err != nil) != tt.invalid {
				t.Fatalf("error = %v", err)
			}
		})
	}
}
