package chatruntime

import (
	"fmt"
	"sort"
	"strings"

	"github.com/airlockrun/agentsdk/capability"
	"github.com/airlockrun/agentsdk/internal/tsrender"
)

const javascriptInstructions = `JavaScript environment:
run_js executes an async function body in an isolated runtime. Use await for every capability call and an explicit return for the result. Each capability accepts one object matching its declared input schema. Console output is bounded.
Scripts execute serially. Within a script, bounded concurrent async callbacks are allowed; await all work before returning. Do not leave background work running.
Local let/const declarations do not persist between scripts. Explicit properties on globalThis may retain data for this uninterrupted run only. Completion, cancellation, or suspension destroys that state; never depend on it across turns or approvals.
The realm has no ambient network, filesystem, process, or platform credentials. Use only the declared capabilities.
user is read-only caller display context (id, email, displayName), or null when the host supplies no human caller. It does not authorize capability calls. air.log is a synchronous alias of console.log with the same bounded output.
Set request_confirmation only for external side effects the user should review (sending, deleting, spending). Explain the effect in description and comment the code for the user. The whole run_js call is approved before any JavaScript executes. Read-only lookups do not need confirmation.
Return only the data needed for the next decision. Share files by reference with air.output; prose belongs in the normal assistant reply.`

// RenderPrompt uses exactly the catalog passed to Run, without discovering or
// independently filtering capabilities. Instructions are host-composed text.
func RenderPrompt(instructions string, catalog []capability.Definition, direct bool) (string, error) {
	if direct {
		return strings.TrimSpace(instructions + "\n\nUse the declared tools. Share files by reference; prose belongs in the normal assistant reply."), nil
	}
	declarations, err := RenderTypeScript(catalog)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(instructions + "\n\n" + javascriptInstructions + "\n\n```typescript\n" + declarations + "```"), nil
}

// RenderTypeScript describes asynchronous bindings from the canonical schemas.
func RenderTypeScript(catalog []capability.Definition) (string, error) {
	tree := map[string]any{}
	for _, d := range catalog {
		parts := d.Path.JSParts()
		if len(parts) == 0 {
			continue
		}
		node := tree
		for _, part := range parts[:len(parts)-1] {
			if node[part] == nil {
				node[part] = map[string]any{}
			}
			node = node[part].(map[string]any)
		}
		node[parts[len(parts)-1]] = d
	}
	var b strings.Builder
	b.WriteString("type FilePath = string;\ntype DirPath = string;\n")
	b.WriteString("declare const user: Readonly<{ id: string; email: string; displayName: string }> | null;\n")
	var render func(map[string]any, string) error
	render = func(node map[string]any, indent string) error {
		names := make([]string, 0, len(node))
		for name := range node {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			switch value := node[name].(type) {
			case map[string]any:
				if indent == "" {
					b.WriteString("declare const ")
				}
				fmt.Fprintf(&b, "%s%s: {\n", indent, name)
				if err := render(value, indent+"  "); err != nil {
					return err
				}
				fmt.Fprintf(&b, "%s};\n", indent)
			case capability.Definition:
				for _, text := range []string{value.Description, value.LLMHint} {
					if text != "" {
						fmt.Fprintf(&b, "%s// %s\n", indent, strings.ReplaceAll(text, "\n", "\n"+indent+"// "))
					}
				}
				for _, ex := range value.InputExamples {
					fmt.Fprintf(&b, "%s// Example: await %s(%s)\n", indent, value.Path.JS(), ex)
				}
				if value.Target == capability.Executor {
					fmt.Fprintf(&b, "%s%s(...values: unknown[]): void;\n", indent, name)
				} else {
					input, err := tsrender.Type(value.InputSchema)
					if err != nil {
						return fmt.Errorf("capability %s input schema: %w", value.Path.ID(), err)
					}
					output, err := tsrender.Type(value.OutputSchema)
					if err != nil {
						return fmt.Errorf("capability %s output schema: %w", value.Path.ID(), err)
					}
					fmt.Fprintf(&b, "%s%s(args: %s): Promise<%s>;\n", indent, name, input, output)
				}
			}
		}
		return nil
	}
	if err := render(tree, ""); err != nil {
		return "", err
	}
	return b.String(), nil
}
