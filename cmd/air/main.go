// Command air authors and maintains Airlock agent repos outside airlock.
//
// It wraps the agentsdk/scaffold package with three subcommands:
//
//	air [init] <dir>             scaffold a new agent into <dir>
//	air update [dir]             regenerate the airlock-managed files in place
//	air toolchain install        install the pinned frontend toolchain
//
// init and update render the same airlock-managed files airlock's builder
// produces; toolchain install fetches the exact templ/tailwind/daisyui versions
// the scaffold pins so local `templ generate` and `tailwindcss` builds match.
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

const defaultBaseImage = "ghcr.io/airlockrun/airlock-agent-base:latest"

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
  air toolchain install [flags]   install the pinned frontend toolchain

Init flags:
  --module <name>            Go module path for the agent (default "agent")
  --agentsdk-version <ver>   agentsdk version to pin (default "v%s")
  --base-image <ref>         runtime base image for the Dockerfile FROM line
                             (default "%s")

Update flags (dir defaults to "."):
  --agentsdk-version <ver>   agentsdk version to pin (default "v%s")
  --base-image <ref>         runtime base image for the Dockerfile FROM line
                             (default "%s")

  Updates the airlock-managed files (Dockerfile, AGENTS.md, %s)
  in place - the external equivalent of airlock's build housekeeping. Run it
  after bumping the agentsdk pin. Requires an existing go.mod in dir.

Toolchain install flags:
  --prefix <dir>             install prefix (default "/usr/local")

  Installs the frontend toolchain pinned by the scaffold:
    templ       %s (via go install)
    tailwindcss %s (standalone binary -> <prefix>/bin)
    daisyui     %s (plugin mjs files -> <prefix>/lib/tailwind)
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

	fmt.Printf("Initialized agent %s in %s\n", id, dir)
	fmt.Printf("  module:   %s\n", f.module)
	fmt.Printf("  agentsdk: %s\n", f.agentSDKVersion)
	fmt.Println("\nNext steps:")
	fmt.Printf("  cd %s\n", dir)
	fmt.Println("  air toolchain install   # install templ + tailwindcss + daisyui")
	fmt.Println("  go mod tidy")
	fmt.Println("  templ generate")
	fmt.Println("  tailwindcss -i styles/app.css -o views/static/app.css --minify")
	fmt.Println("  go build .")
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
	if err := scaffold.GenerateDockerfile(dir, data); err != nil {
		return fmt.Errorf("update Dockerfile: %w", err)
	}
	if err := scaffold.GenerateAgentsMD(dir, data); err != nil {
		return fmt.Errorf("update AGENTS.md: %w", err)
	}
	if err := scaffold.GenerateNotices(dir); err != nil {
		return fmt.Errorf("update notices: %w", err)
	}

	fmt.Printf("Updated airlock-managed files in %s:\n", dir)
	fmt.Println("  Dockerfile")
	fmt.Println("  AGENTS.md")
	fmt.Printf("  %s\n", scaffold.NoticesFilename)
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
	prefix := "/usr/local"
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

	if err := installTempl(); err != nil {
		return err
	}
	if err := installTailwind(prefix); err != nil {
		return err
	}
	if err := installDaisyUI(prefix); err != nil {
		return err
	}

	fmt.Println("\nInstalled frontend toolchain:")
	fmt.Printf("  templ       %s\n", scaffold.TemplVersion)
	fmt.Printf("  tailwindcss %s -> %s\n", scaffold.TailwindVersion, filepath.Join(prefix, "bin", "tailwindcss"))
	fmt.Printf("  daisyui     %s -> %s\n", scaffold.DaisyUIVersion, filepath.Join(prefix, "lib", "tailwind"))
	return nil
}

func installTempl() error {
	pkg := "github.com/a-h/templ/cmd/templ@" + scaffold.TemplVersion
	fmt.Printf("Installing templ %s (go install %s)\n", scaffold.TemplVersion, pkg)
	cmd := exec.Command("go", "install", pkg)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go install templ: %w", err)
	}
	return nil
}

func installTailwind(prefix string) error {
	asset, err := tailwindAsset(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return err
	}
	url := fmt.Sprintf("https://github.com/tailwindlabs/tailwindcss/releases/download/%s/%s",
		scaffold.TailwindVersion, asset)
	dst := filepath.Join(prefix, "bin", "tailwindcss")
	fmt.Printf("Installing tailwindcss %s (%s) -> %s\n", scaffold.TailwindVersion, asset, dst)
	if err := downloadFile(url, dst, 0o755); err != nil {
		return fmt.Errorf("install tailwindcss: %w", err)
	}
	return nil
}

func installDaisyUI(prefix string) error {
	dir := filepath.Join(prefix, "lib", "tailwind")
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

// tailwindAsset maps a Go OS/arch pair to the standalone Tailwind release asset
// name. It is a pure function so it can be unit-tested without networking.
func tailwindAsset(goos, goarch string) (string, error) {
	var osPart string
	switch goos {
	case "linux":
		osPart = "linux"
	case "darwin":
		osPart = "macos"
	default:
		return "", fmt.Errorf("unsupported OS %q for tailwindcss (supported: linux, darwin)", goos)
	}
	var archPart string
	switch goarch {
	case "amd64":
		archPart = "x64"
	case "arm64":
		archPart = "arm64"
	default:
		return "", fmt.Errorf("unsupported architecture %q for tailwindcss (supported: amd64, arm64)", goarch)
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
