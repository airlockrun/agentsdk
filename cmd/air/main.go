// Command air authors and maintains Airlock agent repos outside airlock.
//
// It wraps the agentsdk/scaffold package with three subcommands:
//
//	air [init] <dir>             scaffold a new agent into <dir>
//	air update [dir]             regenerate the airlock-managed files in place
//	go tool air toolchain install
//	                             install the pinned frontend toolchain
//	go tool air build            run the local build chain
//	air login <airlock-url>      store CLI credentials outside the repo
//	air deploy                  upload this repo's source and start a build
//
// init and update render the same airlock-managed files airlock's builder
// produces; toolchain install ensures the pinned templ/tailwind/daisyui versions
// are available so local `go tool templ generate` and repo-local `tailwindcss`
// builds match.
package main

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"github.com/airlockrun/agentsdk"
	"github.com/airlockrun/agentsdk/scaffold"
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
	case "init":
		return cmdInit(args[1:])
	case "update":
		return cmdUpdate(args[1:])
	case "toolchain":
		return cmdToolchain(args[1:])
	case "build":
		return cmdBuild(args[1:])
	case "login":
		return cmdLogin(args[1:])
	case "deploy":
		return cmdDeploy(args[1:])
	default:
		// Bare `air <dir>` is shorthand for `air init <dir>`.
		return cmdInit(args)
	}
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `air - author an Airlock agent repo outside airlock

Usage:
  air [init] <dir> [flags]        scaffold a new agent into <dir>
  air update [dir] [flags]        regenerate the airlock-managed files in place
  air toolchain install [flags]   ensure the pinned frontend toolchain
  air build [dir]                 run the local build chain
  air login <airlock-url>         store CLI credentials outside the repo
  air deploy [dir] [flags]        upload source and start a build

Init flags:
  --module <name>            Go module path for the agent (default "agent")
  --agentsdk-version <ver>   agentsdk version to pin (default "v%s")
  --base-image <ref>         runtime base image for the Dockerfile FROM line
                             (default "%s")
  --airlock <url>            write .airlock/agent.toml with this Airlock URL

Update flags (dir defaults to "."):
  --agentsdk-version <ver>   agentsdk version to pin (default "v%s")
  --base-image <ref>         runtime base image for the Dockerfile FROM line
                             (default "%s")

  Updates the airlock-managed files (Dockerfile, AGENTS.md, %s)
  in place - the external equivalent of airlock's build housekeeping. Run it
  after bumping the agentsdk pin. Requires an existing go.mod in dir.

Toolchain install flags:
  --prefix <dir>             toolchain prefix (default ".airlock/toolchain")

  Ensures the frontend toolchain pinned by the scaffold:
    templ       %s (via go tool templ)
    tailwindcss %s (standalone binary -> <prefix>/bin)
    daisyui     %s (plugin mjs files -> <prefix>/lib/tailwind)

Login flags:
  --no-browser               print the device login URL without opening a browser

  Login uses browser approval with a manually entered device code. User
  authentication happens in Airlock's web UI, including password and passkeys.

Deploy flags:
  --create                   create a draft agent before uploading source
  --slug <slug>              agent slug for --create (derived from --name or dir if omitted)
  --agent <slug-or-id>       existing agent target; overrides .airlock/agent.toml
  --url <url>                Airlock URL; overrides .airlock/agent.toml
  --name <name>              display name for --create (default dir or slug)
  --description <text>       description for --create
`,
		agentsdk.Version, defaultBaseImage,
		agentsdk.Version, defaultBaseImage,
		scaffold.NoticesFilename,
		scaffold.TemplVersion, scaffold.TailwindVersion, scaffold.DaisyUIVersion,
	)
}

// scaffoldFlags holds the flags shared by init and update.
type scaffoldFlags struct {
	module          string
	agentSDKVersion string
	baseImage       string
	airlockURL      string
}

// parseFlags walks a simple `--key value` flag list starting at args, calling
// set for each recognized flag. It returns the non-flag positional arguments.
// We hand-roll this (rather than flag.FlagSet) so a bare `air <dir>` and
// `air init <dir>` share one positional/flag parser and the help text stays a
// single source of truth.
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
		module:          "agent",
		agentSDKVersion: "v" + agentsdk.Version,
		baseImage:       defaultBaseImage,
	}
	positional, err := parseFlags(args, func(key, value string) error {
		switch key {
		case "module":
			f.module = value
		case "agentsdk-version":
			f.agentSDKVersion = value
		case "base-image":
			f.baseImage = value
		case "airlock":
			f.airlockURL = value
		default:
			return fmt.Errorf("unknown flag --%s", key)
		}
		return nil
	})
	return f, positional, err
}

func cmdInit(args []string) error {
	f, positional, err := parseScaffoldFlags(args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return errors.New("init requires exactly one argument: the target directory")
	}
	dir := positional[0]

	if err := ensureEmptyDir(dir); err != nil {
		return err
	}

	id, err := newUUID()
	if err != nil {
		return fmt.Errorf("generate agent id: %w", err)
	}

	data := scaffold.ScaffoldData{
		AgentID:         id,
		Module:          f.module,
		AgentSDKVersion: f.agentSDKVersion,
		AgentBaseImage:  f.baseImage,
	}
	if err := scaffold.Materialize(dir, data); err != nil {
		return fmt.Errorf("materialize agent: %w", err)
	}
	if f.airlockURL != "" {
		if err := writeAgentBinding(dir, agentBinding{AirlockURL: normalizeBaseURL(f.airlockURL)}); err != nil {
			return fmt.Errorf("write agent binding: %w", err)
		}
	}

	fmt.Printf("Initialized agent %s in %s\n", id, dir)
	fmt.Printf("  module:   %s\n", f.module)
	fmt.Printf("  agentsdk: %s\n", f.agentSDKVersion)
	fmt.Println("\nNext steps:")
	fmt.Printf("  cd %s\n", dir)
	fmt.Println("  go mod tidy")
	fmt.Println("  go tool air toolchain install   # install tailwindcss + daisyui")
	fmt.Println("  go tool air build")
	return nil
}

func cmdUpdate(args []string) error {
	f, positional, err := parseScaffoldFlags(args)
	if err != nil {
		return err
	}
	dir := "."
	switch len(positional) {
	case 0:
	case 1:
		dir = positional[0]
	default:
		return errors.New("update takes at most one argument: the target directory")
	}

	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		return fmt.Errorf("no go.mod in %s - update must run in an existing agent repo: %w", dir, err)
	}

	data := scaffold.ScaffoldData{
		Module:          f.module,
		AgentSDKVersion: f.agentSDKVersion,
		AgentBaseImage:  f.baseImage,
	}
	if err := runUpdate(dir, data); err != nil {
		return err
	}

	fmt.Printf("Updated airlock-managed files in %s:\n", dir)
	fmt.Println("  Dockerfile")
	fmt.Println("  AGENTS.md")
	fmt.Printf("  %s\n", scaffold.NoticesFilename)
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
	if err := ensureToolchain(filepath.Join(dir, localToolchainPrefix)); err != nil {
		return err
	}
	tailwindCmd := tailwindBinaryPath(localToolchainPrefix)
	steps := []struct {
		name string
		cmd  []string
	}{
		{"go mod tidy", []string{"go", "mod", "tidy"}},
		{"go tool templ generate", []string{"go", "tool", "templ", "generate"}},
		{"tailwindcss", []string{tailwindCmd, "-i", "styles/app.css", "-o", "views/static/app.css", "--minify"}},
		{"go build", []string{"go", "build", "."}},
	}
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

// ensureEmptyDir creates dir if missing and errors if it exists and is
// non-empty, so scaffolding never clobbers an existing repo.
func ensureEmptyDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return fmt.Errorf("create %s: %w", dir, err)
			}
			return nil
		}
		return fmt.Errorf("read %s: %w", dir, err)
	}
	if len(entries) > 0 {
		return fmt.Errorf("target directory %s is not empty", dir)
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
	prefix := localToolchainPrefix
	if _, err := parseFlags(args, func(key, value string) error {
		switch key {
		case "prefix":
			prefix = value
		default:
			return fmt.Errorf("unknown flag --%s", key)
		}
		return nil
	}); err != nil {
		return err
	}

	if err := ensureToolchain(prefix); err != nil {
		return err
	}

	fmt.Println("\nFrontend toolchain ready:")
	fmt.Printf("  templ       %s -> go tool templ\n", scaffold.TemplVersion)
	fmt.Printf("  tailwindcss %s -> %s\n", scaffold.TailwindVersion, tailwindBinaryPath(prefix))
	fmt.Printf("  daisyui     %s -> %s\n", scaffold.DaisyUIVersion, filepath.Join(prefix, "lib", "tailwind"))
	return nil
}

func ensureToolchain(prefix string) error {
	if toolchainComplete(prefix) {
		return nil
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
	return nil
}

func toolchainCacheDir() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve user cache dir: %w", err)
	}
	return filepath.Join(dir, "airlock", "toolchain"), nil
}

func ensureToolchainCache(cacheDir string) error {
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
	return nil
}

func toolchainComplete(prefix string) bool {
	marker, err := os.ReadFile(filepath.Join(prefix, toolchainMarkerFile))
	if err != nil || string(marker) != toolchainMarker() {
		return false
	}
	for _, path := range []string{
		tailwindBinaryPath(prefix),
		filepath.Join(prefix, "lib", "tailwind", "daisyui.mjs"),
		filepath.Join(prefix, "lib", "tailwind", "daisyui-theme.mjs"),
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
	return fmt.Sprintf("templ=%s\ntailwindcss=%s\ndaisyui=%s\n", scaffold.TemplVersion, scaffold.TailwindVersion, scaffold.DaisyUIVersion)
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
// directories first. A permission error on the target carries a hint to re-run
// with sudo or a writable --prefix.
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
		return fmt.Errorf("%w - %s is not writable; re-run with sudo or pass a writable --prefix", err, filepath.Dir(dst))
	}
	return err
}
