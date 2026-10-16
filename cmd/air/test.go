package main

import (
	"context"
	"errors"
	"fmt"
	airlockv1 "github.com/airlockrun/agentsdk/internal/airlockv1"
	"go/build"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// validateUnitTestImports prevents app tests from silently provisioning the SDK
// simulator. Remote integration packages are explicitly build-tagged and do not
// participate in the default local build/test selection.
func validateUnitTestImports(root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != root && (strings.HasPrefix(entry.Name(), ".") || entry.Name() == "vendor" || entry.Name() == "node_modules" || entry.Name() == "testdata") {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		included, err := build.Default.MatchFile(filepath.Dir(path), entry.Name())
		if err != nil || !included {
			return err
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, spec := range file.Imports {
			name, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			if name == "github.com/airlockrun/agentsdk/agenttest" || name == "github.com/airlockrun/agentsdk/localruntime" || strings.HasPrefix(name, "github.com/testcontainers/") {
				return fmt.Errorf("%s: local app tests cannot import %s; use deterministic domain fixtures or an explicitly tagged remote integration suite", path, name)
			}
		}
		return nil
	})
}

func cmdTest(args []string) error {
	for _, arg := range args {
		if strings.HasPrefix(arg, "--") {
			return cmdRemoteTest(args)
		}
	}
	dir := "."
	if len(args) > 1 {
		return errors.New("air test takes at most one local directory; remote suites require --remote --target --revision")
	}
	if len(args) == 1 {
		dir = args[0]
	}
	if err := validateUnitTestImports(dir); err != nil {
		return err
	}
	command := exec.Command("go", "test", "-p=1", "-count=1", "./...")
	command.Dir = dir
	command.Stdout, command.Stderr = os.Stdout, os.Stderr
	return command.Run()
}

func cmdRemoteTest(args []string) error {
	suite, revision, runID := "integration", "", ""
	var selection []string
	explicitTimeout := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--suite", "--revision", "--run":
			key := args[i]
			if i+1 >= len(args) {
				return fmt.Errorf("%s requires a value", key)
			}
			i++
			switch key {
			case "--suite":
				suite = args[i]
			case "--revision":
				revision = args[i]
			case "--run":
				runID = args[i]
			}
		default:
			if args[i] == "--timeout" {
				explicitTimeout = true
			}
			selection = append(selection, args[i])
		}
	}
	f, positional, err := parseTargetFlags(selection)
	if err != nil {
		return err
	}
	if !deployUUIDRe.MatchString(revision) {
		return errors.New("remote tests require --revision <exact deployed source UUID>")
	}
	if !validRemoteName(suite) {
		return errors.New("invalid suite name")
	}
	if !explicitTimeout {
		f.timeout = 120 * time.Second
	}
	if f.timeout > 600*time.Second || f.timeout%time.Second != 0 {
		return errors.New("suite --timeout must be whole seconds, at most 600s")
	}
	command := "run"
	if len(positional) > 0 {
		if positional[0] == "status" || positional[0] == "cancel" {
			command = positional[0]
			if len(positional) != 1 {
				return errors.New("test status/cancel takes --run UUID, not positional identifiers")
			}
		} else if len(positional) == 1 {
			f.dir = positional[0]
		} else {
			return errors.New("remote test takes at most one directory")
		}
	}
	if command != "run" && !deployUUIDRe.MatchString(runID) {
		return errors.New("test status/cancel requires --run UUID")
	}
	if command == "run" && runID != "" {
		return errors.New("--run requires test status/cancel")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, f.timeout+time.Minute)
	defer cancel()
	s, err := resolveDeployment(ctx, f, false)
	if err != nil {
		return err
	}
	client, err := s.client(revision)
	if err != nil {
		return err
	}
	var info *airlockv1.DeploymentTestRunInfo
	switch command {
	case "run":
		info, err = client.RunTests(ctx, &airlockv1.RunDeploymentTestsRequest{SourceRevisionId: revision, Suite: suite, TimeoutSeconds: int32(f.timeout / time.Second)})
	case "status":
		info, err = client.GetTest(ctx, runID)
	case "cancel":
		info, err = client.CancelTest(ctx, runID)
	}
	if err != nil {
		return err
	}
	if info.DeploymentId != s.info.Id {
		return errors.New("test response selected a different deployment")
	}
	fmt.Fprintf(os.Stderr, "Remote test: %s app=%s target=%s runtime=%s revision=%s\n", info.Id, info.AppId, f.target, info.DeploymentId, info.SourceRevisionId)
	if f.wait {
		info, err = client.WaitTest(ctx, info.Id, 2*time.Second)
		if err != nil {
			return err
		}
	}
	if info.DeploymentId != s.info.Id {
		return errors.New("test observation selected a different deployment")
	}
	if err := writeProtoJSON(&airlockv1.DeploymentTestRunResponse{Test: info}); err != nil {
		return err
	}
	if info.Status == "failed" || info.Status == "cancelled" || (info.Status == "succeeded" && info.ExitCode != 0) {
		return fmt.Errorf("remote suite %s %s (exit %d): %s", info.Suite, info.Status, info.ExitCode, info.Error)
	}
	return nil
}
