package wire

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// SuggestedPrompt is static, editable composer copy, not an executable action.
type SuggestedPrompt struct {
	Title  string `json:"title"`
	Prompt string `json:"prompt"`
}

const (
	MaxSuggestedPrompts          = 6
	MaxSuggestedPromptTitleBytes = 120
	MaxSuggestedPromptBytes      = 4000
)

// ValidateSuggestedPrompts bounds declarations at SDK registration and host admission.
func ValidateSuggestedPrompts(prompts []SuggestedPrompt) error {
	if len(prompts) > MaxSuggestedPrompts {
		return fmt.Errorf("suggested prompts: at most %d declarations", MaxSuggestedPrompts)
	}
	for i, p := range prompts {
		if err := validateSuggestedPromptText(p.Title, p.Prompt); err != nil {
			return fmt.Errorf("suggested prompt %d: %w", i+1, err)
		}
	}
	return nil
}

func validateSuggestedPromptText(title, prompt string) error {
	for _, field := range []struct {
		name, value string
		max         int
	}{{"title", title, MaxSuggestedPromptTitleBytes}, {"prompt", prompt, MaxSuggestedPromptBytes}} {
		if !utf8.ValidString(field.value) || strings.TrimSpace(field.value) == "" || len(field.value) > field.max {
			return fmt.Errorf("%s must contain nonblank UTF-8 text of at most %d bytes", field.name, field.max)
		}
		for _, r := range field.value {
			if unicode.IsControl(r) && !(field.name == "prompt" && (r == '\n' || r == '\t')) {
				return fmt.Errorf("%s contains a control character", field.name)
			}
		}
	}
	return nil
}
