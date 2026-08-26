// Command air authors and maintains Airlock agent repos outside airlock.
//
// It wraps the agentsdk/scaffold package with local build and source-sync commands:
//
//	air init <dir>               scaffold a new agent into <dir>
//	air update [dir]             reconcile module pins, managed files, and toolchain
//	go tool air toolchain install
//	                             install the pinned build toolchain
//	go tool air build            run the local build chain
//	air login <airlock-url>      store CLI credentials outside the repo
//	air logout <airlock-url>     revoke and remove CLI credentials
//	air deploy -m "Fix retries"  upload this repo's source and start a build
//	air pull                     fast-forward this workspace from Airlock
//	air clone <agent> <dir>      clone Airlock source without Git
//
// init and update render the same airlock-managed files airlock's builder
// produces; toolchain install ensures the pinned templ/sqlc/tailwind/daisyui versions
// and their coding skills are available so local harnesses and builds match
// airlock codegen.
package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"github.com/airlockrun/agentsdk"
	"github.com/airlockrun/agentsdk/internal/bootstrap"
	"github.com/airlockrun/agentsdk/scaffold"
	"golang.org/x/mod/modfile"
)

const (
	defaultBaseImage     = "ghcr.io/airlockrun/airlock-agent-base:latest"
	localToolchainPrefix = ".airlock/toolchain"
	toolchainMarkerFile  = "air-toolchain.version"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "air: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage(os.Stderr)
		return errors.New("no subcommand given")
	}

	switch args[0] {
	case "-h", "--help", "help":
		usage(os.Stdout)
		return nil
	case "--version", "version":
		fmt.Printf("air v%s\n", agentsdk.Version)
		return nil
	case "init":
		return cmdInit(args[1:])
	case "update":
		return cmdUpdate(args[1:])
	case "toolchain":
		return cmdToolchain(args[1:])
	case "build":
		return cmdBuild(args[1:])
	case "integrations":
		return cmdIntegrations(args[1:])
	case "connection":
		return cmdConnection(args[1:])
	case "exec":
		return cmdExec(args[1:])
	case "mcp":
		return cmdMCP(args[1:])
	case "login":
		return cmdLogin(args[1:])
	case "logout":
		return cmdLogout(args[1:])
	case "deploy":
		return cmdDeploy(args[1:])
	case "pull":
		return cmdPull(args[1:])
	case "clone":
		return cmdClone(args[1:])
	case "remote":
		return cmdRemote(args[1:])
	default:
		return fmt.Errorf("unknown command %q; run 'air help' for usage", args[0])
	}
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `air v%s - author an Airlock agent repo outside airlock

Usage:
  air version                     print the selected CLI version
  air init <dir> [flags]          scaffold a new agent into <dir>
  air update [dir]                update module pins, managed files, and toolchain
  air toolchain install           ensure the pinned build tools and references
  air build [dir]                 run the local build chain
  air integrations list [flags]   list configured external integrations
  air connection request ...      call a target's HTTP connection
  air exec run ...                 run a command on a target's exec endpoint
  air mcp probe|tools|call ...     inspect or call MCP servers
  air login <airlock-url>         store CLI credentials outside the repo
  air logout <airlock-url>        revoke and remove CLI credentials
  air deploy [dir] -m <text>      upload source and start a build
  air pull [dir] [flags]          fast-forward a local workspace from Airlock
  air clone <agent> <dir> [flags] clone Airlock source without Git
  air remote default <name>       select the default deployment target
  air remote unbind <name>        clear a remote's local agent binding

Init flags:
  --agentsdk-version <ver>   agentsdk version to pin (default "v%s")
  --url <url>                write .airlock/local/agent.toml with this Airlock URL

Update (dir defaults to "."):
  Reconciles go.mod with this air version, updates the airlock-managed files
  (Dockerfile, AGENTS.md, .gitignore, %s), runs go mod tidy, and refreshes
  .airlock/toolchain. Requires an existing go.mod in dir.

Toolchain install:
  Ensures the build toolchain pinned by the scaffold:
    templ       %s (via go tool templ)
    sqlc        %s (standalone binary -> .airlock/toolchain/bin)
    tailwindcss %s (standalone binary -> .airlock/toolchain/bin)
    daisyui     %s (plugin mjs files -> .airlock/toolchain/lib/tailwind)
    references     (agentsdk + UI docs -> .airlock/toolchain/skills)

Login flags:
  --reauthenticate            start a new device login even if already logged in
  --no-browser               print the device login URL without opening a browser
  --no-wait                  start device login, save pending state, and exit
  --wait                     wait for browser approval even without a TTY
  --check                    check one pending device login once and exit

  Login validates and reuses working credentials saved for the Airlock URL.
  Rejected credentials start a new browser approval; --reauthenticate starts
  one even when the saved login works. In a TTY, air login waits for approval.
  Without a TTY, it behaves like --no-wait so code harnesses do not hang on a
  foreground poll loop.

Deploy flags:
  --create                   create a draft agent before uploading source
  --slug <slug>              agent slug for --create (derived from --name or dir if omitted)
  --agent <slug-or-id>       existing agent target for a new or matching remote
  --url <url>                Airlock URL for a new or matching remote
  --remote <name>            named deployment target (default: configured default_remote)
  --name <name>              display name for --create (default dir or slug)
  --description <text>       description for --create
  -m, --message <text>       required build message (single line, 200 bytes max)
  --force                    replace stale Airlock source intentionally

Pull flags:
  --agent <slug-or-id>       existing agent target for a new or matching remote
  --url <url>                Airlock URL for a new or matching remote
  --remote <name>            named deployment target (default: configured default_remote)
  --force                    discard local source changes

Clone flags:
  --url <url>                Airlock URL; defaults to the sole saved login
  --remote <name>            name stored as the cloned workspace's target

Integration target flags:
  --agent <slug-or-id>       agent for a new or matching remote
  --url <url>                Airlock URL for a new or matching remote
  --remote <name>            named deployment target (default: configured default_remote)

A remote binds one Airlock URL and one stable agent ID. Selecting a remote
does not change default_remote. Use air remote default <name> to change it.
Unbinding clears only the local agent ID, slug, and source state; it preserves
the Airlock URL and does not delete or modify an agent in Airlock.
`,
		agentsdk.Version,
		agentsdk.Version,
		scaffold.NoticesFilename,
		scaffold.TemplVersion, scaffold.SQLCVersion, scaffold.TailwindVersion, scaffold.DaisyUIVersion,
	)
}

// scaffoldFlags holds the inputs used to materialize a scaffold.
type scaffoldFlags struct {
	agentSDKVersion string
	url             string
}

// parseFlags walks a simple `--key value` flag list starting at args, calling
// set for each recognized flag. It returns the non-flag positional arguments.
// We hand-roll this (rather than flag.FlagSet) so positional arguments and
// flags can appear in either order and the help text stays a single source of
// truth.
func parseFlags(args []string, set func(key, value string) error) ([]string, error) {
	var positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if len(a) < 2 || a[:2] != "--" {
			positional = append(positional, a)
			continue
		}
		key := a[2:]
		if i+1 >= len(args) {
			return nil, fmt.Errorf("flag --%s needs a value", key)
		}
		i++
		if err := set(key, args[i]); err != nil {
			return nil, err
		}
	}
	return positional, nil
}

func parseScaffoldFlags(args []string) (scaffoldFlags, []string, error) {
	f := scaffoldFlags{
		agentSDKVersion: "v" + agentsdk.Version,
	}
	positional, err := parseFlags(args, func(key, value string) error {
		switch key {
		case "agentsdk-version":
			f.agentSDKVersion = value
		case "url":
			f.url = value
		default:
			return fmt.Errorf("unknown flag --%s", key)
		}
		return nil
	})
	return f, positional, err
}

func cmdInit(args []string) error {
	return runInit(args, tidyModule)
}

func runInit(args []string, tidy func(string) error) error {
	f, positional, err := parseScaffoldFlags(args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return errors.New("init requires exactly one argument: the target directory")
	}
	dir := positional[0]

	if err := bootstrap.ResetDir(dir); err != nil {
		return err
	}

	id, err := newUUID()
	if err != nil {
		return fmt.Errorf("generate agent id: %w", err)
	}

	data := scaffold.ScaffoldData{
		AgentID:         id,
		AgentSDKVersion: f.agentSDKVersion,
		AgentBaseImage:  defaultBaseImage,
	}
	if err := scaffold.Materialize(dir, data); err != nil {
		return fmt.Errorf("materialize agent: %w", err)
	}
	if f.url != "" {
		binding := agentBinding{}
		binding.putRemote(defaultRemoteName, agentRemoteBinding{AirlockURL: normalizeBaseURL(f.url)})
		if err := writeAgentBinding(dir, binding); err != nil {
			return fmt.Errorf("write agent binding: %w", err)
		}
	}
	if err := tidy(dir); err != nil {
		return err
	}

	fmt.Printf("Initialized agent %s in %s\n", id, dir)
	fmt.Printf("  agentsdk: %s\n", f.agentSDKVersion)
	fmt.Println("\nNext steps:")
	fmt.Printf("  cd %s\n", dir)
	fmt.Println("  go tool air toolchain install   # install build tools + coding skills")
	fmt.Println("  go tool air build")
	return nil
}

func cmdRemote(args []string) error {
	if len(args) != 2 || (args[0] != "default" && args[0] != "unbind") {
		return errors.New("remote requires: default <name> or unbind <name>")
	}
	binding, ok, err := loadAgentBinding(".")
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("workspace is not bound to Airlock; deploy or clone it first")
	}
	name := args[1]
	switch args[0] {
	case "default":
		if err := binding.setDefaultRemote(name); err != nil {
			return err
		}
		if err := writeAgentBinding(".", binding); err != nil {
			return err
		}
		fmt.Printf("Default remote is now %s\n", name)
	case "unbind":
		remote, found := binding.remote(name)
		if !found {
			return fmt.Errorf("remote %q is not defined in %s", name, agentBindingPath)
		}
		if remote.AgentID == "" {
			return fmt.Errorf("remote %q is not bound to an agent", name)
		}
		agentID, slug := remote.AgentID, remote.Slug
		remote.AgentID = ""
		remote.Slug = ""
		remote.SourceState = ""
		binding.Remotes[name] = remote
		if err := writeAgentBinding(".", binding); err != nil {
			return err
		}
		agent := agentID
		if slug != "" {
			agent = fmt.Sprintf("%s (%s)", slug, agentID)
		}
		fmt.Printf("Remote %q is no longer bound to %s; Airlock URL preserved at %s\n", name, agent, remote.AirlockURL)
	}
	return nil
}

func cmdUpdate(args []string) error {
	for _, arg := range args {
		if strings.HasPrefix(arg, "--") {
			return fmt.Errorf("unknown update flag %s", arg)
		}
	}
	dir := "."
	switch len(args) {
	case 0:
	case 1:
		dir = args[0]
	default:
		return errors.New("update takes at most one argument: the target directory")
	}
	return runUpdateCommand(dir, tidyModule, ensureToolchain)
}

func runUpdateCommand(dir string, tidy func(string) error, install func(string, string) error) error {
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		return fmt.Errorf("no go.mod in %s - update must run in an existing agent repo: %w", dir, err)
	}
	version := runningAgentSDKVersion()
	if err := reconcileAgentModule(dir, version); err != nil {
		return err
	}
	if err := tidy(dir); err != nil {
		return err
	}
	data := scaffold.ScaffoldData{
		AgentSDKVersion: version,
		AgentBaseImage:  defaultBaseImage,
	}
	if err := runUpdate(dir, data); err != nil {
		return err
	}
	if err := install(dir, filepath.Join(dir, localToolchainPrefix)); err != nil {
		return err
	}

	fmt.Printf("Updated agent workspace in %s:\n", dir)
	fmt.Println("  go.mod and go.sum")
	fmt.Println("  Dockerfile")
	fmt.Println("  AGENTS.md")
	fmt.Println("  .gitignore")
	fmt.Printf("  %s\n", scaffold.NoticesFilename)
	fmt.Println("  .airlock/toolchain")
	return nil
}

func runningAgentSDKVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		if info.Main.Path == "github.com/airlockrun/agentsdk" && info.Main.Version != "" && info.Main.Version != "(devel)" {
			return info.Main.Version
		}
		for _, dep := range info.Deps {
			if dep.Path == "github.com/airlockrun/agentsdk" && dep.Version != "" && dep.Version != "(devel)" {
				return dep.Version
			}
		}
	}
	return "v" + agentsdk.Version
}

func reconcileAgentModule(dir, agentSDKVersion string) error {
	path := filepath.Join(dir, "go.mod")
	body, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	mf, err := modfile.Parse(path, body, nil)
	if err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	var hasAgentSDK bool
	for _, require := range mf.Require {
		if require.Mod.Path == "github.com/airlockrun/agentsdk" {
			hasAgentSDK = true
			break
		}
	}
	if !hasAgentSDK {
		return fmt.Errorf("%s: no require directive for github.com/airlockrun/agentsdk", path)
	}
	if err := mf.AddGoStmt(scaffold.GoVersion); err != nil {
		return fmt.Errorf("set Go version: %w", err)
	}
	if err := mf.AddRequire("github.com/a-h/templ", scaffold.TemplVersion); err != nil {
		return fmt.Errorf("require templ %s: %w", scaffold.TemplVersion, err)
	}
	if err := mf.AddRequire("github.com/airlockrun/agentsdk", agentSDKVersion); err != nil {
		return fmt.Errorf("require agentsdk %s: %w", agentSDKVersion, err)
	}
	for _, tool := range []string{
		"github.com/a-h/templ/cmd/templ",
		"github.com/airlockrun/agentsdk/cmd/air",
	} {
		if err := mf.AddTool(tool); err != nil {
			return fmt.Errorf("add tool %s: %w", tool, err)
		}
	}
	updated, err := mf.Format()
	if err != nil {
		return fmt.Errorf("format %s: %w", path, err)
	}
	if string(updated) == string(body) {
		return nil
	}
	if err := os.WriteFile(path, updated, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func runUpdate(dir string, data scaffold.ScaffoldData) error {
	if err := scaffold.GenerateDockerfile(dir, data); err != nil {
		return fmt.Errorf("update Dockerfile: %w", err)
	}
	if err := scaffold.GenerateAgentsMD(dir, data); err != nil {
		return fmt.Errorf("update AGENTS.md: %w", err)
	}
	if err := scaffold.GenerateNotices(dir); err != nil {
		return fmt.Errorf("update notices: %w", err)
	}
	if err := reconcileGeneratedArtifactIgnores(dir); err != nil {
		return fmt.Errorf("update .gitignore: %w", err)
	}
	return nil
}

func reconcileGeneratedArtifactIgnores(dir string) error {
	ignorePath := filepath.Join(dir, ".gitignore")
	body, err := os.ReadFile(ignorePath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	present := make(map[string]bool)
	for _, line := range strings.Split(string(body), "\n") {
		present[strings.TrimSpace(line)] = true
	}
	var missing []string
	for _, pattern := range scaffold.GeneratedArtifactIgnorePatterns() {
		if !present[pattern] {
			missing = append(missing, pattern)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	out := string(body)
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	out += strings.Join(missing, "\n") + "\n"
	return os.WriteFile(ignorePath, []byte(out), 0o644)
}

func tidyModule(dir string) error {
	cmd := exec.Command("go", "mod", "tidy")
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tidy module dependencies: %w", err)
	}
	return nil
}

func cmdBuild(args []string) error {
	dir := "."
	switch len(args) {
	case 0:
	case 1:
		dir = args[0]
	default:
		return errors.New("build takes at most one argument: the agent repo directory")
	}
	return runBuild(dir)
}

func runBuild(dir string) error {
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		return fmt.Errorf("build requires an agent repo with go.mod in %s: %w", dir, err)
	}
	if err := ensureToolchain(dir, filepath.Join(dir, localToolchainPrefix)); err != nil {
		return err
	}
	if err := cleanGeneratedDBFiles(dir); err != nil {
		return fmt.Errorf("clean generated sqlc output: %w", err)
	}
	outputDir, err := os.MkdirTemp("", "air-agent-build-*")
	if err != nil {
		return fmt.Errorf("create build output directory: %w", err)
	}
	defer os.RemoveAll(outputDir)
	outputName := "agent"
	if runtime.GOOS == "windows" {
		outputName += ".exe"
	}

	tailwindCmd := tailwindBinaryPath(localToolchainPrefix)
	sqlcCmd := sqlcBinaryPath(localToolchainPrefix)
	steps := buildSteps(sqlcCmd, tailwindCmd, filepath.Join(outputDir, outputName), hasSQLQueries(dir))
	for _, step := range steps {
		fmt.Printf("==> %s\n", step.name)
		cmd := exec.Command(step.cmd[0], step.cmd[1:]...)
		cmd.Dir = dir
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s: %w", step.name, err)
		}
	}
	return nil
}

type buildStep struct {
	name string
	cmd  []string
}

func buildSteps(sqlcCmd, tailwindCmd, outputPath string, generateSQLC bool) []buildStep {
	steps := []buildStep{{"go mod tidy", []string{"go", "mod", "tidy"}}}
	if generateSQLC {
		steps = append(steps, buildStep{"sqlc generate", []string{sqlcCmd, "generate"}})
	}
	return append(steps,
		buildStep{"go tool templ generate", []string{"go", "tool", "templ", "generate"}},
		buildStep{"tailwindcss", []string{tailwindCmd, "-i", "styles/app.css", "-o", "views/static/app.css", "--minify"}},
		buildStep{"go test -p=1 -count=1 ./...", []string{"go", "test", "-p=1", "-count=1", "./..."}},
		buildStep{"go build", []string{"go", "build", "-buildvcs=false", "-o", outputPath, "."}},
	)
}

func hasSQLQueries(dir string) bool {
	matches, err := filepath.Glob(filepath.Join(dir, "db", "queries", "*.sql"))
	return err == nil && len(matches) > 0
}

func cleanGeneratedDBFiles(dir string) error {
	dbDir := filepath.Join(dir, "internal", "db")
	entries, err := os.ReadDir(dbDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !scaffold.IsGeneratedArtifact(filepath.ToSlash(filepath.Join("internal", "db", entry.Name()))) {
			continue
		}
		if err := os.Remove(filepath.Join(dbDir, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

// newUUID returns a random RFC 4122 version 4 UUID string in lowercase
// 8-4-4-4-12 hex form. It uses crypto/rand so we avoid a uuid dependency.
func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

func cmdToolchain(args []string) error {
	if len(args) == 0 {
		return errors.New("toolchain requires a subcommand: install")
	}
	switch args[0] {
	case "install":
		return cmdInstallToolchain(args[1:])
	default:
		return fmt.Errorf("unknown toolchain subcommand %q", args[0])
	}
}

func cmdInstallToolchain(args []string) error {
	if len(args) != 0 {
		return errors.New("toolchain install takes no arguments")
	}
	prefix := localToolchainPrefix

	if err := ensureToolchain(".", prefix); err != nil {
		return err
	}

	fmt.Println("\nBuild toolchain ready:")
	fmt.Printf("  templ       %s -> go tool templ\n", scaffold.TemplVersion)
	fmt.Printf("  sqlc        %s -> %s\n", scaffold.SQLCVersion, sqlcBinaryPath(prefix))
	fmt.Printf("  tailwindcss %s -> %s\n", scaffold.TailwindVersion, tailwindBinaryPath(prefix))
	fmt.Printf("  daisyui     %s -> %s\n", scaffold.DaisyUIVersion, filepath.Join(prefix, "lib", "tailwind"))
	fmt.Printf("  UI refs     %s -> %s\n", scaffold.SkillsDigest(), filepath.Join(prefix, "skills"))
	fmt.Printf("  agentsdk    %s -> %s\n", runningAgentSDKVersion(), filepath.Join(prefix, "skills", "agentsdk"))
	return nil
}

func ensureToolchain(projectDir, prefix string) error {
	moduleDir, err := agentSDKModuleDir(projectDir)
	if err != nil {
		return err
	}
	return ensureToolchainFromModule(prefix, moduleDir)
}

func ensureToolchainFromModule(prefix, moduleDir string) error {
	if toolchainComplete(prefix) {
		return installAgentSDKSkill(filepath.Join(prefix, "skills", "agentsdk"), moduleDir)
	}
	cacheDir, err := toolchainCacheDir()
	if err != nil {
		return err
	}
	if err := ensureToolchainCache(cacheDir); err != nil {
		return err
	}
	if err := projectToolchain(prefix, cacheDir); err != nil {
		return err
	}
	if err := writeToolchainMarker(prefix); err != nil {
		return err
	}
	if !toolchainComplete(prefix) {
		return fmt.Errorf("projected toolchain into %s, but required files are still missing", prefix)
	}
	return installAgentSDKSkill(filepath.Join(prefix, "skills", "agentsdk"), moduleDir)
}

func agentSDKModuleDir(projectDir string) (string, error) {
	cmd := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "github.com/airlockrun/agentsdk")
	cmd.Dir = projectDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("resolve agentsdk module directory: %w: %s", err, strings.TrimSpace(string(out)))
	}
	dir := strings.TrimSpace(string(out))
	if dir == "" {
		return "", errors.New("resolve agentsdk module directory: go list returned an empty directory")
	}
	return dir, nil
}

func installAgentSDKSkill(dst, moduleDir string) error {
	root, err := os.ReadFile(filepath.Join(moduleDir, "REFERENCE.md"))
	if err != nil {
		return fmt.Errorf("read agentsdk reference: %w", err)
	}
	entries, err := os.ReadDir(filepath.Join(moduleDir, "reference"))
	if err != nil {
		return fmt.Errorf("read agentsdk companion references: %w", err)
	}
	parent := filepath.Dir(dst)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(parent, ".agentsdk-skill-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	header := fmt.Sprintf("---\nname: agentsdk\ndescription: Airlock Agents SDK API and runtime reference. TRIGGER when writing or changing agent Go code.\nmetadata:\n  version: %s\n---\n\n", runningAgentSDKVersion())
	if err := os.WriteFile(filepath.Join(tmp, "SKILL.md"), append([]byte(header), root...), 0o644); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(tmp, "reference"), 0o755); err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".md" {
			continue
		}
		if err := copyFile(
			filepath.Join(moduleDir, "reference", entry.Name()),
			filepath.Join(tmp, "reference", entry.Name()),
			0o644,
		); err != nil {
			return fmt.Errorf("copy agentsdk reference %s: %w", entry.Name(), err)
		}
	}
	backup := dst + ".old"
	if err := os.RemoveAll(backup); err != nil {
		return err
	}
	if _, err := os.Stat(dst); err == nil {
		if err := os.Rename(dst, backup); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Rename(backup, dst)
		return err
	}
	return os.RemoveAll(backup)
}

func toolchainCacheDir() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve user cache dir: %w", err)
	}
	return filepath.Join(dir, "airlock", "toolchain"), nil
}

func ensureToolchainCache(cacheDir string) error {
	if _, err := os.Stat(sqlcCachePath(cacheDir)); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		if err := installSQLC(cacheDir); err != nil {
			return err
		}
	}
	if _, err := os.Stat(tailwindCachePath(cacheDir)); err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		if err := installTailwind(cacheDir); err != nil {
			return err
		}
	}
	for _, file := range []string{"daisyui.mjs", "daisyui-theme.mjs"} {
		if _, err := os.Stat(filepath.Join(daisyUICacheDir(cacheDir), file)); err != nil {
			if !os.IsNotExist(err) {
				return err
			}
			if err := installDaisyUI(cacheDir); err != nil {
				return err
			}
			break
		}
	}
	return nil
}

func projectToolchain(prefix, cacheDir string) error {
	links := map[string]struct {
		src  string
		perm os.FileMode
	}{
		sqlcBinaryPath(prefix):     {src: sqlcCachePath(cacheDir), perm: 0o755},
		tailwindBinaryPath(prefix): {src: tailwindCachePath(cacheDir), perm: 0o755},
		filepath.Join(prefix, "lib", "tailwind", "daisyui.mjs"): {
			src:  filepath.Join(daisyUICacheDir(cacheDir), "daisyui.mjs"),
			perm: 0o644,
		},
		filepath.Join(prefix, "lib", "tailwind", "daisyui-theme.mjs"): {
			src:  filepath.Join(daisyUICacheDir(cacheDir), "daisyui-theme.mjs"),
			perm: 0o644,
		},
	}
	for dst, link := range links {
		if err := linkOrCopy(link.src, dst, link.perm); err != nil {
			return err
		}
	}
	if err := scaffold.InstallSkills(filepath.Join(prefix, "skills")); err != nil {
		return fmt.Errorf("install coding skills: %w", err)
	}
	return nil
}

func toolchainComplete(prefix string) bool {
	marker, err := os.ReadFile(filepath.Join(prefix, toolchainMarkerFile))
	if err != nil || string(marker) != toolchainMarker() {
		return false
	}
	for _, path := range []string{
		sqlcBinaryPath(prefix),
		tailwindBinaryPath(prefix),
		filepath.Join(prefix, "lib", "tailwind", "daisyui.mjs"),
		filepath.Join(prefix, "lib", "tailwind", "daisyui-theme.mjs"),
		filepath.Join(prefix, "skills", "daisyui", "SKILL.md"),
		filepath.Join(prefix, "skills", "templ", "SKILL.md"),
		filepath.Join(prefix, "skills", "htmx", "SKILL.md"),
		filepath.Join(prefix, "skills", "lucide", "SKILL.md"),
		filepath.Join(prefix, "skills", "lucide", "reference", "icons.md"),
	} {
		if _, err := os.Stat(path); err != nil {
			return false
		}
	}
	return true
}

func writeToolchainMarker(prefix string) error {
	path := filepath.Join(prefix, toolchainMarkerFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(toolchainMarker()), 0o644)
}

func toolchainMarker() string {
	return fmt.Sprintf("templ=%s\nsqlc=%s\ntailwindcss=%s\ndaisyui=%s\nskills=%s\n", scaffold.TemplVersion, scaffold.SQLCVersion, scaffold.TailwindVersion, scaffold.DaisyUIVersion, scaffold.SkillsDigest())
}

func installSQLC(cacheDir string) error {
	asset, err := sqlcAsset(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return err
	}
	archivePath := filepath.Join(cacheDir, "sqlc", scaffold.SQLCVersion, runtime.GOOS+"-"+runtime.GOARCH, asset)
	url := fmt.Sprintf("https://github.com/sqlc-dev/sqlc/releases/download/v%s/%s", scaffold.SQLCVersion, asset)
	fmt.Printf("Installing sqlc %s (%s) -> %s\n", scaffold.SQLCVersion, asset, sqlcCachePath(cacheDir))
	if err := downloadFile(url, archivePath, 0o644); err != nil {
		return fmt.Errorf("install sqlc: %w", err)
	}
	defer os.Remove(archivePath)
	if err := extractSQLCBinary(archivePath, sqlcCachePath(cacheDir)); err != nil {
		return fmt.Errorf("install sqlc: %w", err)
	}
	return nil
}

func installTailwind(cacheDir string) error {
	asset, err := tailwindAsset(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("https://github.com/tailwindlabs/tailwindcss/releases/download/%s/%s",
		scaffold.TailwindVersion, asset)
	dst := tailwindCachePath(cacheDir)
	fmt.Printf("Installing tailwindcss %s (%s) -> %s\n", scaffold.TailwindVersion, asset, dst)
	if err := downloadFile(url, dst, 0o755); err != nil {
		return fmt.Errorf("install tailwindcss: %w", err)
	}
	return nil
}

func installDaisyUI(cacheDir string) error {
	dir := daisyUICacheDir(cacheDir)
	for _, file := range []string{"daisyui.mjs", "daisyui-theme.mjs"} {
		url := fmt.Sprintf("https://github.com/saadeghi/daisyui/releases/download/%s/%s",
			scaffold.DaisyUIVersion, file)
		dst := filepath.Join(dir, file)
		fmt.Printf("Installing daisyui %s (%s) -> %s\n", scaffold.DaisyUIVersion, file, dst)
		if err := downloadFile(url, dst, 0o644); err != nil {
			return fmt.Errorf("install %s: %w", file, err)
		}
	}
	return nil
}

func tailwindBinaryPath(prefix string) string {
	return filepath.Join(prefix, "bin", tailwindBinaryName())
}

func sqlcBinaryPath(prefix string) string {
	return filepath.Join(prefix, "bin", sqlcBinaryName())
}

func sqlcCachePath(cacheDir string) string {
	return filepath.Join(cacheDir, "sqlc", scaffold.SQLCVersion, runtime.GOOS+"-"+runtime.GOARCH, sqlcBinaryName())
}

func tailwindCachePath(cacheDir string) string {
	return filepath.Join(cacheDir, "tailwindcss", scaffold.TailwindVersion, runtime.GOOS+"-"+runtime.GOARCH, tailwindBinaryName())
}

func daisyUICacheDir(cacheDir string) string {
	return filepath.Join(cacheDir, "daisyui", scaffold.DaisyUIVersion)
}

func tailwindBinaryName() string {
	name := "tailwindcss"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

func sqlcBinaryName() string {
	name := "sqlc"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

func linkOrCopy(src, dst string, perm os.FileMode) error {
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("cached toolchain file %s missing: %w", src, err)
	}
	if filepath.Clean(dst) == filepath.Clean(src) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Symlink(src, dst); err == nil {
		return nil
	}
	return copyFile(src, dst, perm)
}

func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			os.Remove(dst)
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	if err := os.Chmod(dst, perm); err != nil {
		return err
	}
	ok = true
	return nil
}

func sqlcAsset(goos, goarch string) (string, error) {
	switch goos {
	case "linux", "darwin", "windows":
	default:
		return "", fmt.Errorf("unsupported OS %q for sqlc (supported: linux, darwin, windows)", goos)
	}
	if goarch != "amd64" && goarch != "arm64" {
		return "", fmt.Errorf("unsupported architecture %q for sqlc (supported: amd64, arm64)", goarch)
	}
	if goos == "windows" && goarch == "arm64" {
		return "", errors.New("unsupported architecture \"arm64\" for sqlc on windows (supported: amd64)")
	}
	ext := ".tar.gz"
	if goos == "windows" {
		ext = ".zip"
	}
	return fmt.Sprintf("sqlc_%s_%s_%s%s", scaffold.SQLCVersion, goos, goarch, ext), nil
}

func extractSQLCBinary(archivePath, dst string) error {
	if strings.HasSuffix(archivePath, ".zip") {
		zr, err := zip.OpenReader(archivePath)
		if err != nil {
			return err
		}
		defer zr.Close()
		for _, file := range zr.File {
			if pathBase(file.Name) != sqlcBinaryName() {
				continue
			}
			r, err := file.Open()
			if err != nil {
				return err
			}
			err = writeExtractedBinary(dst, r)
			r.Close()
			return err
		}
		return errors.New("sqlc archive does not contain the sqlc binary")
	}

	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return errors.New("sqlc archive does not contain the sqlc binary")
		}
		if err != nil {
			return err
		}
		if pathBase(header.Name) == sqlcBinaryName() {
			return writeExtractedBinary(dst, tr)
		}
	}
}

func pathBase(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		return name[i+1:]
	}
	return name
}

func writeExtractedBinary(dst string, src io.Reader) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return writeHint(err, dst)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".sqlc-*")
	if err != nil {
		return writeHint(err, dst)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := io.Copy(tmp, src); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return writeHint(err, dst)
	}
	return nil
}

// tailwindAsset maps a Go OS/arch pair to the standalone Tailwind release asset
// name. It is a pure function so it can be unit-tested without networking.
func tailwindAsset(goos, goarch string) (string, error) {
	var osPart string
	switch goos {
	case "linux":
		osPart = "linux"
	case "darwin":
		osPart = "macos"
	case "windows":
		osPart = "windows"
	default:
		return "", fmt.Errorf("unsupported OS %q for tailwindcss (supported: linux, darwin, windows)", goos)
	}
	var archPart string
	switch goarch {
	case "amd64":
		archPart = "x64"
	case "arm64":
		if goos == "windows" {
			return "", errors.New("unsupported architecture \"arm64\" for tailwindcss on windows (supported: amd64)")
		}
		archPart = "arm64"
	default:
		return "", fmt.Errorf("unsupported architecture %q for tailwindcss (supported: amd64, arm64)", goarch)
	}
	if goos == "windows" {
		return fmt.Sprintf("tailwindcss-%s-%s.exe", osPart, archPart), nil
	}
	return fmt.Sprintf("tailwindcss-%s-%s", osPart, archPart), nil
}

// downloadFile fetches url and writes it to dst with mode perm, creating parent
// directories first. A permission error identifies the managed directory that
// must be writable.
func downloadFile(url, dst string, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return writeHint(err, dst)
	}

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: unexpected status %s", url, resp.Status)
	}

	tmp, err := os.CreateTemp(filepath.Dir(dst), ".air-*")
	if err != nil {
		return writeHint(err, dst)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if _, err := io.Copy(tmp, resp.Body); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", dst, err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return writeHint(err, dst)
	}
	return nil
}

func writeHint(err error, dst string) error {
	if errors.Is(err, os.ErrPermission) {
		return fmt.Errorf("%w - managed toolchain directory %s is not writable", err, filepath.Dir(dst))
	}
	return err
}
