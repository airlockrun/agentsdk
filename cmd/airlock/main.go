// Command airlock bootstraps and dispatches the repository-pinned Air CLI.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/airlockrun/agentsdk"
	airlockv1 "github.com/airlockrun/agentsdk/internal/airlockv1"
	"github.com/airlockrun/agentsdk/internal/bootstrap"
	"golang.org/x/mod/modfile"
	"golang.org/x/mod/semver"
	"google.golang.org/protobuf/encoding/protojson"
)

const sdkInfoPath = "/.well-known/airlock-agent-sdk"

var (
	httpClient = &http.Client{Timeout: 30 * time.Second}
	execute    = executeCommand
	loadInfo   = fetchSDKInfo
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "airlock: %v\n", err)
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
		fmt.Printf("airlock v%s\n", agentsdk.Version)
		return nil
	case "init":
		return cmdInit(args[1:])
	case "clone":
		return cmdClone(args[1:])
	default:
		return delegate(args)
	}
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `airlock v%s - bootstrap the repository-pinned Air CLI

Usage:
  airlock init <dir> --url <url>
  airlock clone <agent> --url <url> <dir> [--remote <name>]
  airlock <command> [args...]   delegate from an agent repo to go tool air
`, agentsdk.Version)
}

func cmdInit(args []string) error {
	positional, flags, err := parseArgs(args, map[string]bool{"url": true})
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return errors.New("init requires exactly one argument: the target directory")
	}
	baseURL, err := validateBaseURL(flags["url"])
	if err != nil {
		return err
	}
	dir := positional[0]
	if err := prepareTool(dir, baseURL); err != nil {
		return err
	}
	return execute(dir, "go", "tool", "air", "init", ".", "--url", baseURL)
}

func cmdClone(args []string) error {
	positional, flags, err := parseArgs(args, map[string]bool{"url": true, "remote": true})
	if err != nil {
		return err
	}
	if len(positional) != 2 {
		return errors.New("clone requires an agent slug-or-id and destination directory")
	}
	baseURL, err := validateBaseURL(flags["url"])
	if err != nil {
		return err
	}
	dir := positional[1]
	if err := prepareTool(dir, baseURL); err != nil {
		return err
	}
	if err := execute(dir, "go", "tool", "air", "login", baseURL, "--wait"); err != nil {
		return err
	}
	toolArgs := []string{"tool", "air", "clone", positional[0], ".", "--url", baseURL}
	if flags["remote"] != "" {
		toolArgs = append(toolArgs, "--remote", flags["remote"])
	}
	return execute(dir, "go", toolArgs...)
}

func parseArgs(args []string, allowed map[string]bool) ([]string, map[string]string, error) {
	flags := make(map[string]string)
	var positional []string
	for i := 0; i < len(args); i++ {
		if !strings.HasPrefix(args[i], "--") {
			positional = append(positional, args[i])
			continue
		}
		key := strings.TrimPrefix(args[i], "--")
		if !allowed[key] {
			return nil, nil, fmt.Errorf("unknown flag --%s", key)
		}
		if i+1 >= len(args) {
			return nil, nil, fmt.Errorf("flag --%s needs a value", key)
		}
		i++
		flags[key] = args[i]
	}
	return positional, flags, nil
}

func validateBaseURL(value string) (string, error) {
	if value == "" {
		return "", errors.New("--url is required")
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("invalid Airlock URL %q: use an http or https base URL", value)
	}
	return strings.TrimRight(value, "/"), nil
}

func prepareTool(dir, baseURL string) error {
	info, err := loadInfo(context.Background(), baseURL)
	if err != nil {
		return err
	}
	version := "v" + strings.TrimPrefix(info.GetVersion(), "v")
	if !semver.IsValid(version) {
		return fmt.Errorf("Airlock returned invalid Agent SDK version %q", info.GetVersion())
	}
	if info.GetCommandImport() != bootstrap.ToolImport {
		return fmt.Errorf("Airlock returned unsupported Air CLI import %q; update the global launcher:\n  go install github.com/airlockrun/agentsdk/cmd/airlock@%s", info.GetCommandImport(), version)
	}
	if err := bootstrap.InitializeDir(dir, version); err != nil {
		return err
	}
	if err := execute(dir, "go", "get", "-tool", bootstrap.ToolImport+"@"+version); err != nil {
		return fmt.Errorf("select Air CLI %s: %w", version, err)
	}
	isBootstrap, err := bootstrap.EnsureDir(dir)
	if err != nil {
		return err
	}
	if !isBootstrap {
		return errors.New("go get did not produce an Airlock bootstrap module")
	}
	return nil
}

func fetchSDKInfo(ctx context.Context, baseURL string) (*airlockv1.GetAgentSDKInfoResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+sdkInfoPath, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("read Airlock SDK metadata: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("read Airlock SDK metadata: %s", resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read Airlock SDK metadata: %w", err)
	}
	var info airlockv1.GetAgentSDKInfoResponse
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("decode Airlock SDK metadata: %w", err)
	}
	return &info, nil
}

func delegate(args []string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	modPath, err := findGoMod(cwd)
	if err != nil {
		return err
	}
	body, err := os.ReadFile(modPath)
	if err != nil {
		return err
	}
	mf, err := modfile.Parse(modPath, body, nil)
	if err != nil {
		return fmt.Errorf("parse %s: %w", modPath, err)
	}
	for _, tool := range mf.Tool {
		if tool.Path == bootstrap.ToolImport {
			return execute(cwd, "go", append([]string{"tool", "air"}, args...)...)
		}
	}
	return fmt.Errorf("%s does not declare tool %s; outside an agent repo, airlock supports only init and clone", modPath, bootstrap.ToolImport)
}

func findGoMod(dir string) (string, error) {
	for {
		path := filepath.Join(dir, "go.mod")
		if _, err := os.Stat(path); err == nil {
			return path, nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("not in an agent repository; airlock supports only init and clone here")
		}
		dir = parent
	}
}

func executeCommand(dir, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
