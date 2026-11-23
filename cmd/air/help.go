package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/airlockrun/agentsdk"
)

// commandHelp describes public commands, not worker protocols.
type commandHelp struct {
	Command, Usage, Summary, Flags, SideEffects string
}

type flagHelp struct {
	Name         string `json:"name"`
	TakesValue   bool   `json:"takesValue"`
	ValueSyntax  string `json:"valueSyntax,omitempty"`
	Description  string `json:"description"`
	Default      string `json:"default,omitempty"`
	Required     bool   `json:"required"`
	RequiredWhen string `json:"requiredWhen,omitempty"`
	Group        string `json:"group"`
}

type optionDefinition struct{ Value, Description string }

// Option types drive normalization, payload protection, validation and discovery.
var optionDefinitions = map[string]optionDefinition{
	"--json":             {"", "Emit one versioned JSON result, including errors; diagnostics go to stderr."},
	"--dir":              {"DIR", "Application repository directory."},
	"--remote":           {"NAME", "Use this saved remote, retaining its application binding."},
	"--url":              {"URL", "Airlock API origin; must agree with a selected saved remote."},
	"--agent":            {"APP", "Select an application by slug or UUID instead of the remote's binding."},
	"--target":           {"NAME", "Select production, the saved preview, or a concrete target name/UUID; creating a preview does not change the laptop default."},
	"--timeout":          {"DURATION", "Bound this command's execution/observation; stopping observation does not cancel remote work."},
	"--agentsdk-version": {"VERSION", "Published SDK version to pin in the new repository."},
	"--slug":             {"SLUG", "Application slug for --create (2–27 lowercase letters, digits and single dashes)."},
	"--name":             {"NAME", "Display name for the created application or connector."},
	"--description":      {"TEXT", "Application description; requires --create."},
	"--message":          {"MESSAGE", "Nonblank single-line source submission message; alias: -m."},
	"-m":                 {"MESSAGE", "Nonblank single-line source submission message; alias: --message."},
	"--bind":             {"[TYPE:]SLOT=UUID[@FINGERPRINT]", "Select a resource for a candidate slot; repeat for multiple slots. A fingerprint acknowledges the exact server-owned binding contract."},
	"--fixture":          {"NAME", "Select a source-declared fixture."},
	"--build":            {"UUID", "Observe this exact build rather than the latest build/task."},
	"--limit":            {"N", "Maximum number of recent builds to list."},
	"--input":            {"JSON", "JSON input passed to the tool or agent."},
	"--data":             {"BODY", "Request body, or JSON arguments for an MCP call."},
	"--header":           {"NAME:VALUE", "Add a request header; repeat for multiple headers."},
	"--request-id":       {"UUID", "Exact admission identity; preserve it and the payload after uncertainty, never invent a replacement to retry."},
	"--contract":         {"HASH", "Expected server-owned agent contract hash."},
	"--revision":         {"UUID", "Pin the revision required by this operation."},
	"--prompt":           {"TEXT", "Prompt for the selected continuation."},
	"--resource":         {"ADDRESS", "Global resource address (/name or /UUID) to bind."},
	"--method":           {"METHOD", "HTTP request method."},
	"--path":             {"/PATH", "Relative resource HTTP path."},
	"--tool":             {"NAME", "MCP tool name to invoke."},
	"--type":             {"TYPE", "Disambiguate resource slots with the same name."},
	"--acknowledge":      {"FINGERPRINT", "Acknowledge only the exact server fingerprint confirmed by the human."},
	"--host":             {"UUID", "Exact enrolled host; authorization is scoped to that host."},
	"--connector":        {"UUID", "Exact connector to update."},
	"--need":             {"SLUG", "Declared application connector need."},
	"--artifact":         {"UUID", "Published connector artifact file."},
	"--settings-file":    {"PATH", "Read connector settings from a local JSON file."},
	"--cwd":              {"DIR", "Working directory on the enrolled host."},
	"--env":              {"KEY=VALUE", "Host command environment entry; repeat for multiple entries."},
	"--stdin-file":       {"PATH", "Read host command stdin from a local file."},
	"--suite":            {"NAME", "Source-declared remote test suite."},
	"--run":              {"UUID", "Exact existing remote test run."},
	"--launch":           {"", "Resume an existing stopped preview after source admission; a new build owns startup. Does not imply --wait."},
	"--wait":             {"", "Observe completion within the timeout; does not launch a stopped runtime or cancel work on timeout."},
	"--no-wait":          {"", "Submit without waiting for completion."},
	"--create":           {"", "Create and bind a new application before uploading; use for the first preview in an unbound repository. Cannot combine with --agent."},
	"--force":            {"", "DANGER: replace the source CAS baseline with the server's current source before uploading; can overwrite another caller's update. Never an automatic retry."},
	"--logs":             {"", "Include logs for the observed build."},
	"--reauthenticate":   {"", "Start a fresh browser authorization instead of using saved credentials."},
	"--no-browser":       {"", "Print the approval URL/code without opening a browser."},
	"--check":            {"", "Check the existing pending browser authorization without starting another one."},
}

type commandDiscovery struct {
	Command     string     `json:"command"`
	Usage       string     `json:"usage"`
	Summary     string     `json:"summary"`
	Flags       []flagHelp `json:"flags"`
	SideEffects string     `json:"sideEffects"`
	Constraints []string   `json:"constraints,omitempty"`
}

const selectionHelp = "--dir DIR, --remote NAME, --url URL, --agent SLUG-OR-ID, --target NAME, --timeout DURATION"
const selectionNoTargetHelp = "--dir DIR, --remote NAME, --url URL, --agent SLUG-OR-ID, --timeout DURATION"
const remoteHelp = "--remote NAME, --url URL, --agent APP, --target NAME"
const submitHelp = "-m MESSAGE, --message MESSAGE, --remote NAME, --url URL, --agent APP, --wait, --timeout DURATION, --create, --name NAME, --slug SLUG, --description TEXT, --force, --bind [TYPE:]SLOT=UUID[@FINGERPRINT], --fixture [TYPE:]SLOT=NAME"

var publicCommands = []commandHelp{
	{"version", "version", "Print the CLI version.", "", "none"},
	{"init", "init DIR", "Create an application repository.", "--agentsdk-version VERSION, --url URL", "local files and downloads"},
	{"update", "update [DIR]", "Reconcile managed files, module pins and build tools.", "", "local files and downloads"},
	{"toolchain install", "toolchain install", "Install pinned build tools and coding references.", "", "local files and downloads"},
	{"build", "build [DIR]", "Generate, test and compile locally.", "", "local files and subprocesses"},
	{"test", "test [DIR] | test --revision UUID --suite NAME [--wait]", "Run local tests or an exact server-owned revision's remote suite.", selectionHelp + ", --revision UUID, --suite NAME, --wait", "tests or remote test admission"},
	{"test status", "test status --run UUID --revision UUID", "Observe an existing remote test run.", selectionHelp + ", --run UUID, --revision UUID, --wait", "read"},
	{"test cancel", "test cancel --run UUID --revision UUID", "Request cancellation, not rollback.", selectionHelp + ", --run UUID, --revision UUID, --wait", "remote cancellation"},
	{"login", "login URL", "Approve browser login. Non-interactive use submits without waiting.", "--reauthenticate, --no-browser, --no-wait, --wait, --check", "credentials and remote login"},
	{"logout", "logout URL", "Revoke and remove the saved login.", "", "credentials and remote revocation"},
	{"remote default", "remote default NAME", "Select the local default remote.", "", "local selection"},
	{"remote unbind", "remote unbind NAME", "Clear only the local app binding; retain the remote URL.", "", "local selection"},
	{"clone", "clone APP DIR", "Download selected application source.", "--url URL, --remote NAME, --target NAME", "local files"},
	{"pull", "pull [DIR]", "Fast-forward local source from the selected target.", remoteHelp + ", --force", "local files; --force discards local changes"},
	{"preview", "preview [DIR] -m MESSAGE", "Submit to the saved preview. Acceptance is not readiness.", submitHelp + ", --launch", "source admission, optional app creation and local preview/source selection"},
	{"preview select", "preview select TARGET", "Explicitly select an existing preview.", selectionNoTargetHelp, "local preview selection"},
	{"preview configuration", "preview configuration REVISION", "Inspect or set pending candidate selections.", selectionNoTargetHelp + ", --bind [TYPE:]SLOT=UUID[@FINGERPRINT], --fixture [TYPE:]SLOT=NAME", "read; selection flags change candidate configuration"},
	{"deploy", "deploy [DIR] -m MESSAGE", "Submit to a selected target; production is the laptop default.", submitHelp + ", --target NAME", "source admission, optional app creation and local source state"},
	{"deploy list", "deploy list [DIR]", "List recent builds.", remoteHelp + ", --limit N, --timeout DURATION", "read"},
	{"deploy status", "deploy status [DIR]", "Observe one pinned build; completion does not prove runtime readiness.", remoteHelp + ", --build UUID, --wait, --timeout DURATION, --logs", "read and local task reconciliation"},
	{"status", "status [DIR]", "Show readiness; --wait observes the admitted update and active running image. --revision pins a serving canonical revision, not a pending candidate.", selectionHelp + ", --wait, --revision UUID", "read and local task reconciliation"},
	{"inspect", "inspect [DIR]", "Show deployment identities, source, image and ownership diagnostics. --revision requires that exact serving canonical revision.", selectionHelp + ", --revision UUID", "read"},
	{"start", "start", "Start the selected runtime.", selectionHelp + ", --wait", "runtime lifecycle"},
	{"stop", "stop", "Stop the selected runtime.", selectionHelp, "runtime lifecycle"},
	{"restart", "restart", "Restart in one operation.", selectionHelp + ", --wait", "runtime lifecycle"},
	{"suspend", "suspend", "Suspend the selected runtime.", selectionHelp, "runtime lifecycle"},
	{"env list", "env list", "List declarations without displaying secrets.", remoteHelp, "read"},
	{"env get", "env get SLUG", "Print a declared non-secret value.", remoteHelp, "read"},
	{"env set", "env set SLUG VALUE", "Save a declared value; inspect the target for activation and restart requirements.", remoteHelp, "target configuration"},
	{"env clear", "env clear SLUG", "Clear a declared value; inspect the target for activation and restart requirements.", remoteHelp, "target configuration"},
	{"request", "request METHOD /PATH", "Request authenticated HTTP; stdout is raw body by default.", selectionHelp + ", --data BODY, --header NAME:VALUE, --revision UUID", "application HTTP execution"},
	{"tools list", "tools list", "List app tools.", selectionHelp + ", --revision UUID", "read"},
	{"tools invoke", "tools invoke NAME --input JSON", "Invoke an app tool.", selectionHelp + ", --input JSON, --revision UUID", "application tool execution"},
	{"agent start", "agent start DEFINITION --input JSON", "Admit a run; retain request identity after uncertainty.", selectionHelp + ", --input JSON, --request-id UUID, --contract HASH, --revision UUID, --wait", "agent execution"},
	{"agent list", "agent list DEFINITION", "List definition runs.", selectionHelp + ", --revision UUID", "read"},
	{"agent get", "agent get DEFINITION RUN", "Inspect one run.", selectionHelp + ", --revision UUID", "read"},
	{"agent wait", "agent wait DEFINITION RUN", "Observe an existing run without resubmission.", selectionHelp + ", --revision UUID", "read"},
	{"agent cancel", "agent cancel DEFINITION RUN", "Request cancellation, not rollback.", selectionHelp + ", --revision UUID", "agent cancellation"},
	{"agent continue", "agent continue DEFINITION SESSION --prompt TEXT", "Continue a session with an exact request identity.", selectionHelp + ", --prompt TEXT, --request-id UUID, --contract HASH, --revision UUID", "agent execution"},
	{"logs", "logs RUN", "Read raw run log snapshot.", selectionHelp + ", --revision UUID", "read"},
	{"resources list", "resources list [TARGET]", "Discover all resource types, including connectors; with TARGET, list declared app slots instead.", selectionHelp, "read"},
	{"resources inspect", "resources inspect ADDRESS", "Inspect /global-name, /UUID or TARGET/SLOT.", selectionHelp + ", --type TYPE", "read"},
	{"resources request", "resources request ADDRESS --path /PATH", "Request a resource; stdout is raw body by default.", selectionHelp + ", --type TYPE, --method METHOD, --path /PATH, --data BODY, --header NAME:VALUE", "resource execution"},
	{"resources mcp-tools", "resources mcp-tools ADDRESS", "Discover authorized MCP tools.", selectionHelp + ", --type TYPE", "resource execution"},
	{"resources mcp-call", "resources mcp-call ADDRESS --tool NAME", "Call an authorized MCP tool.", selectionHelp + ", --type TYPE, --tool NAME, --data JSON", "resource execution"},
	{"resources mcp-probe", "resources mcp-probe URL", "Inspect public MCP/OAuth discovery without credentials.", "", "public network reads"},
	{"resources bindings", "resources bindings --target TARGET", "Inspect current target declarations, not pending candidate declarations.", selectionHelp, "read"},
	{"resources candidates", "resources candidates TARGET/SLOT", "Inspect server-owned compatibility, optionally for an exact pending candidate.", selectionHelp + ", --type TYPE, --revision UUID", "read"},
	{"resources bind", "resources bind TARGET/SLOT --resource /NAME", "Bind a resource; acknowledge only a human-confirmed server fingerprint.", selectionHelp + ", --type TYPE, --resource ADDRESS, --acknowledge FINGERPRINT", "target configuration"},
	{"resources fixture", "resources fixture TARGET/SLOT --fixture NAME --revision UUID", "Select a source-owned HTTP fixture.", selectionHelp + ", --type TYPE, --fixture NAME, --revision UUID", "target configuration"},
	{"resources unbind", "resources unbind TARGET/SLOT", "Remove a resource selection.", selectionHelp + ", --type TYPE", "target configuration"},
	{"connectors inspect", "connectors inspect ID", "Diagnose an installation: host, artifact version/digest and management state. Discover with resources list.", "--url URL", "read"},
	{"connectors install", "connectors install --host UUID --agent UUID --need SLUG --artifact UUID --name NAME", "Install; default waits for verified inventory.", "--host UUID, --agent UUID, --need SLUG, --artifact UUID, --name NAME, --url URL, --request-id UUID, --settings-file PATH, --wait, --no-wait, --timeout DURATION", "host management"},
	{"connectors update", "connectors update --host UUID --connector UUID --agent UUID --need SLUG --artifact UUID", "Update; default waits for verified inventory.", "--host UUID, --connector UUID, --agent UUID, --need SLUG, --artifact UUID, --url URL, --request-id UUID, --settings-file PATH, --wait, --no-wait, --timeout DURATION", "host management"},
	{"hosts exec", "hosts exec HOST -- COMMAND [ARG...]", "Execute on an exact host; retain request ID and payload after uncertainty.", "--url URL, --cwd DIR, --env KEY=VALUE, --stdin-file PATH, --timeout DURATION, --request-id UUID, --wait, --no-wait", "host command execution"},
	{"hosts job", "hosts job JOB", "Inspect a durable job.", "--url URL, --wait, --no-wait", "read"},
	{"hosts cancel", "hosts cancel JOB", "Request cancellation, not rollback.", "--url URL, --wait, --no-wait", "host job cancellation"},
	{"hosts request", "hosts request HOST REQUEST", "Recover exact admission without replay.", "--url URL, --wait, --no-wait", "read"},
}

func declaredFlags(command commandHelp) []flagHelp {
	flags := []flagHelp{describeFlag(command.Command, "--json", "")}
	for _, syntax := range strings.Split(command.Flags, ",") {
		words := strings.Fields(syntax)
		if len(words) == 0 {
			continue
		}
		value := strings.Join(words[1:], " ")
		takesValue := optionTakesValue(words[0])
		if takesValue != (value != "") {
			panic("public command flag has inconsistent value syntax: " + syntax)
		}
		flags = append(flags, describeFlag(command.Command, words[0], value))
	}
	return flags
}

func describeFlag(command, name, value string) flagHelp {
	definition, ok := optionDefinitions[name]
	if !ok {
		panic("missing option definition: " + name)
	}
	f := flagHelp{Name: name, TakesValue: definition.Value != "", ValueSyntax: value, Description: definition.Description, Group: "common"}
	if !f.TakesValue {
		f.Default = "false"
	}
	switch name {
	case "--force", "--bind", "--fixture", "--slug", "--name", "--description", "--agent", "--url", "--revision", "--contract", "--settings-file", "--acknowledge":
		f.Group = "advanced"
	}
	switch name {
	case "--dir":
		f.Default = "."
	case "--target":
		f.Default = "production on laptop; injected preview in hosted development"
	case "--remote":
		f.Default = "saved default remote, otherwise default"
	case "--timeout":
		f.Default = "10m"
		if strings.HasPrefix(command, "test") {
			f.Default = "2m for remote suites"
		}
		if strings.HasPrefix(command, "connectors") || command == "hosts exec" {
			f.Default = "5m execution plus 30s observation settlement"
		}
	case "--wait":
		f.Default = "false"
		if command == "login" {
			f.Default = "true only on a TTY"
		}
		if strings.HasPrefix(command, "connectors") || strings.HasPrefix(command, "hosts") {
			f.Default = "true"
		}
	case "--input":
		f.Default = "{}"
	case "--method":
		f.Default = "GET"
	case "--force":
		if command == "pull" {
			f.Description = "DANGER: discard local changes while pulling the selected target's source."
		}
	case "--fixture":
		if command == "preview" || command == "deploy" || command == "preview configuration" {
			f.Description = "Select a source-declared HTTP fixture for a candidate slot ([TYPE:]SLOT=NAME); repeat for multiple slots."
		}
	case "--revision":
		if command == "status" || command == "inspect" {
			f.Description = "Require this exact canonical serving source revision, not an uploaded candidate UUID; fail on mismatch without reconciling unrelated cached tasks."
		}
	}
	if name == "-m" || name == "--message" {
		f.RequiredWhen = "preview/deploy: one of -m or --message is required"
	}
	if name == "--suite" {
		f.RequiredWhen = "running a remote test suite"
	}
	if name == "--revision" && strings.HasPrefix(command, "test") {
		f.RequiredWhen = "remote test operations"
		if command != "test" {
			f.Required = true
			f.RequiredWhen = ""
		}
	}
	if command == "resources fixture" && (name == "--revision" || name == "--fixture") {
		f.Required = true
	}
	if strings.HasPrefix(command, "connectors") && name == "--agent" {
		f.Description = "Exact application UUID declaring the connector need."
	}
	if name == "--run" || name == "--resource" || name == "--path" || name == "--tool" || name == "--prompt" || name == "--host" || name == "--connector" || name == "--need" || name == "--artifact" {
		f.Required = true
	}
	if command == "connectors install" && (name == "--name" || name == "--agent") || command == "connectors update" && name == "--agent" {
		f.Required = true
	}
	return f
}

func helpConstraints(command string) []string {
	switch command {
	case "resources mcp-probe":
		return []string{"Public MCP/OAuth discovery uses the positional URL without credentials; it has no saved remote, application or target selectors."}
	case "resources list":
		return []string{"resources list discovers global user/remote resources, including connectors and readiness; --agent is unavailable. resources list preview lists that app's preview slots instead. A positional target or --target selects slot inventory.", "Use resources inspect /NAME for safe resource details and resources candidates preview/SLOT --type connector for compatible selections. View, binding/use and installation management have independent authorization."}
	case "resources bindings":
		return []string{"Read the selected target's known declarations, not a pending candidate's declarations. No global address is accepted."}
	case "resources candidates", "resources bind", "resources fixture", "resources unbind":
		return []string{"The positional address must be TARGET/SLOT, not /global-name. --target must agree with TARGET. Binding and fixture selection use the selected target/candidate's declared slots."}
	case "connectors", "connectors install", "connectors update", "connectors inspect":
		return []string{"Discovery and binding belong to resources list/inspect/candidates/bind. connectors operates on installations; hosts job/request/cancel observes or recovers management work without replay.", "An update changes the installed executable/settings, not just an app binding. Binding/use grants no management authority. A multi-target need can bind a target group containing several installations; each installation is reserved to one need.", "Install/update requires exact-host manage access and an authorized retained app/need artifact. Update compatibility includes the installation's management app/need and bound interfaces; a binding does not transfer ownership.", "Hosted developer install/update uses the existing human confirmation for host_management_v1: exact host, workspace and human, valid for one hour. approval_required (77) means no protected effect; resolve the pending confirmation in the developer conversation, not by granting access from the CLI or replaying the whole shell."}
	}
	if command == "resources" || strings.HasPrefix(command, "resources ") {
		return []string{"Global addresses (/name or /UUID) need no app deployment: use --url or a saved remote. Do not pass --agent or --target for global resources.", "Target/slot addresses use that target's known declarations; --target must agree with the address. Codegen credentials support only the injected runtime's slots, not global inventory."}
	}
	return nil
}

func findPublicCommand(args []string) (commandHelp, int, bool) {
	var found commandHelp
	consumed := 0
	for _, command := range publicCommands {
		if len(args) > 0 && args[0] == "--version" && command.Command == "version" {
			return command, 1, true
		}
		words := strings.Fields(command.Command)
		if len(words) > consumed && len(args) >= len(words) && strings.Join(args[:len(words)], " ") == command.Command {
			found, consumed = command, len(words)
		}
	}
	return found, consumed, consumed != 0
}

func validateCommandFlags(command commandHelp, args []string) error {
	allowed := map[string]flagHelp{}
	for _, flag := range declaredFlags(command) {
		allowed[flag.Name] = flag
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return nil
		}
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		flag, ok := allowed[arg]
		if !ok {
			return fmt.Errorf("unknown %s flag %q; run 'air help %s'", command.Command, arg, command.Command)
		}
		if flag.TakesValue {
			if i+1 >= len(args) {
				return fmt.Errorf("%s requires %s", arg, flag.ValueSyntax)
			}
			i++
		}
	}
	return nil
}

func helpRequest(args []string) (string, bool) {
	if len(args) == 0 {
		return "", false
	}
	if args[0] == "help" {
		var path []string
		for _, arg := range args[1:] {
			if arg != "--help" && arg != "-h" {
				path = append(path, arg)
			}
		}
		return strings.Join(path, " "), true
	}
	value := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if value {
			value = false
			continue
		}
		if arg == "--" {
			break
		}
		if optionTakesValue(arg) {
			value = true
			continue
		}
		if arg == "--help" || arg == "-h" {
			var words []string
			for _, word := range args[:i] {
				if strings.HasPrefix(word, "-") {
					break
				}
				words = append(words, word)
			}
			if len(words) > 0 && !knownHelp(words[0]) {
				return strings.Join(words, " "), true
			}
			for len(words) > 0 && !knownHelp(strings.Join(words, " ")) {
				words = words[:len(words)-1]
			}
			return strings.Join(words, " "), true
		}
	}
	return "", false
}

func optionTakesValue(name string) bool {
	return optionDefinitions[name].Value != ""
}

func knownHelp(path string) bool {
	for _, item := range publicCommands {
		if item.Command == path || strings.HasPrefix(item.Command, path+" ") {
			return true
		}
	}
	return path == ""
}

func printHelp(w io.Writer, path string, machine bool) error {
	var matches []commandHelp
	for _, item := range publicCommands {
		if path == "" || item.Command == path || strings.HasPrefix(item.Command, path+" ") {
			matches = append(matches, item)
		}
	}
	if len(matches) == 0 {
		return fmt.Errorf("unknown help topic %q", path)
	}
	if machine {
		commands := make([]commandDiscovery, 0, len(matches))
		for _, command := range matches {
			commands = append(commands, commandDiscovery{Command: command.Command, Usage: command.Usage, Summary: command.Summary, Flags: declaredFlags(command), SideEffects: command.SideEffects, Constraints: helpConstraints(command.Command)})
		}
		var guidance []cliAction
		if action := localHelpAction(path); action.Executable != "" {
			guidance = append(guidance, action)
		}
		return json.NewEncoder(w).Encode(map[string]any{"schemaVersion": 1, "command": "help", "status": "completed", "data": map[string]any{"version": agentsdk.Version, "commands": commands}, "guidance": guidance})
	}
	if path == "" {
		fmt.Fprintf(w, "air v%s — build and operate Airlock applications\n\n", agentsdk.Version)
		fmt.Fprintln(w, "Local:     init · update · toolchain install · build · test\nAccount:   login · logout · remote · clone · pull\nDeploy:    preview · deploy · status · inspect · start · stop · restart · suspend\nApp:       env · request · tools · agent · logs\nResources: resources · connectors · hosts\n\nUsage: air help COMMAND [SUBCOMMAND]\n       air COMMAND --help")
		fmt.Fprintln(w, "Output: add --json for one machine-readable result, including errors.")
		return nil
	}
	for _, item := range matches {
		fmt.Fprintf(w, "Usage: air %s\n%s\n", item.Usage, item.Summary)
		if item.Command == path {
			for _, group := range []string{"common", "advanced"} {
				printed := false
				for _, flag := range declaredFlags(item) {
					if flag.Group != group {
						continue
					}
					if !printed {
						if group == "common" {
							fmt.Fprintln(w, "Options:")
						} else {
							fmt.Fprintln(w, "Advanced / selection options:")
						}
						printed = true
					}
					label := strings.TrimSpace(flag.Name + " " + flag.ValueSyntax)
					fmt.Fprintf(w, "  %-32s %s", label, flag.Description)
					if flag.Default != "" {
						if flag.Default == "." {
							fmt.Fprint(w, " Default: '.'.")
						} else {
							fmt.Fprintf(w, " Default: %s.", flag.Default)
						}
					}
					if flag.Required {
						fmt.Fprint(w, " Required.")
					}
					if flag.RequiredWhen != "" {
						fmt.Fprintf(w, " Required: %s.", flag.RequiredWhen)
					}
					fmt.Fprintln(w)
				}
			}
		}
		fmt.Fprintf(w, "Effects: %s\n\n", item.SideEffects)
	}
	fmt.Fprintln(w, "Output: --json emits one schemaVersion:1 result document, including errors; diagnostics use stderr. Value flags accept --flag=value. Payload arguments after -- are unchanged.")
	switch path {
	case "preview":
		fmt.Fprintln(w, "First preview (after login and choosing a remote/API origin):\n  go tool air preview --create --url https://api.example.test -m 'First preview' --launch\nExisting app:\n  go tool air preview -m 'Describe changes' --launch\nObserve readiness separately with status --target preview --wait; --launch is not --wait.")
	case "status":
		fmt.Fprintln(w, "Example: go tool air status --target preview --wait --timeout 10m")
	case "deploy status":
		fmt.Fprintln(w, "Example: go tool air deploy status --target preview --wait --timeout 10m --logs")
	case "resources":
		fmt.Fprintln(w, "Example: go tool air resources list\n         go tool air resources inspect /personal_api")
	}
	for _, constraint := range helpConstraints(path) {
		fmt.Fprintln(w, constraint)
	}
	switch path {
	case "preview", "deploy", "deploy status", "status", "agent start", "agent wait":
		fmt.Fprintln(w, "Observation timeout defaults to 10m.")
	case "start", "restart":
		fmt.Fprintln(w, "Observation timeout defaults to 10m; it concerns the current runtime, separately from deployment update outcome.")
	case "connectors install", "connectors update", "hosts exec":
		fmt.Fprintln(w, "Execution timeout defaults to 5m; bounded observation allows 30s for delivery settlement. --no-wait submits only.")
	case "hosts job", "hosts request", "hosts cancel":
		fmt.Fprintln(w, "--wait is bounded by the saved job deadline plus 30s delivery settlement; interruption does not cancel execution.")
	case "test", "test status", "test cancel":
		fmt.Fprintln(w, "Remote suite timeout defaults to 2m, at most 10m in whole seconds; observation allows 1m settlement. Local tests use Go's test runner.")
	}
	if strings.Contains(path, "status") || path == "preview" || path == "deploy" || strings.Contains(path, "wait") {
		fmt.Fprintln(w, "Waiting is bounded observation, not cancellation. A timeout may leave the operation running. Resume observation; do not resubmit. Use built-in bounded waits rather than hand-written status polling loops.")
	}
	return nil
}

// normalizeOptions leaves payloads after -- untouched.
func normalizeOptions(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			return append(out, args[i:]...)
		}
		if optionTakesValue(arg) && i+1 < len(args) {
			out = append(out, arg, args[i+1])
			i++
			continue
		}
		if strings.HasPrefix(arg, "--") {
			if key, value, ok := strings.Cut(arg, "="); ok && optionTakesValue(key) {
				out = append(out, key, value)
				continue
			}
		}
		out = append(out, arg)
	}
	return out
}
