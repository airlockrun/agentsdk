package main

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/exp/apidiff"
	"golang.org/x/mod/semver"
	"golang.org/x/tools/go/packages"
)

var publicPackages = []string{
	"github.com/airlockrun/agentsdk",
	"github.com/airlockrun/agentsdk/agenttest",
	"github.com/airlockrun/goai",
	"github.com/airlockrun/goai/message",
	"github.com/airlockrun/goai/model",
	"github.com/airlockrun/goai/output",
	"github.com/airlockrun/goai/schema",
	"github.com/airlockrun/goai/stream",
	"github.com/airlockrun/goai/tool",
	"github.com/airlockrun/sol/websearch",
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "API compatibility check failed:", err)
		os.Exit(1)
	}
}

func run() error {
	root, err := commandOutput("", "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	root = strings.TrimSpace(root)

	current, err := packageVersion(filepath.Join(root, "agentsdk.go"))
	if err != nil {
		return err
	}
	tagsOutput, err := commandOutput(root, "git", "tag", "--list", "v*")
	if err != nil {
		return err
	}
	currentTag := "v" + current
	tagsAtHead, err := commandOutput(root, "git", "tag", "--points-at", "HEAD", "--list", currentTag)
	if err != nil {
		return err
	}
	baseline, breaking, err := selectBaseline(strings.Fields(tagsOutput), currentTag, strings.TrimSpace(tagsAtHead) == currentTag)
	if err != nil {
		return err
	}

	oldRoot, err := os.MkdirTemp("", "agentsdk-api-old-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(oldRoot)
	if err := extractTag(root, baseline, oldRoot); err != nil {
		return err
	}

	var incompatible []string
	for _, path := range publicPackages {
		oldPkg, err := loadPackage(oldRoot, path)
		if err != nil {
			return fmt.Errorf("load %s from %s: %w", path, baseline, err)
		}
		newPkg, err := loadPackage(root, path)
		if err != nil {
			return fmt.Errorf("load current %s: %w", path, err)
		}
		report := apidiff.Changes(oldPkg.Types, newPkg.Types)
		for _, change := range report.Changes {
			message := fmt.Sprintf("%s: %s", path, change.Message)
			if change.Compatible || ignoredMetadataChange(path, change.Message) {
				fmt.Println(message)
				continue
			}
			incompatible = append(incompatible, message)
		}
	}

	if len(incompatible) == 0 {
		fmt.Printf("API compatibility: %s is compatible with %s\n", current, baseline)
		return nil
	}
	sort.Strings(incompatible)
	for _, message := range incompatible {
		fmt.Fprintln(os.Stderr, message)
	}
	if breaking {
		fmt.Printf("API compatibility: allowing pre-1.0 prerelease %s against %s\n", current, baseline)
		return nil
	}
	return fmt.Errorf("%d incompatible API change(s) in compatibility series %s", len(incompatible), compatibilitySeries("v"+current))
}

func packageVersion(path string) (string, error) {
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		return "", err
	}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range value.Names {
				if name.Name != "Version" || i >= len(value.Values) {
					continue
				}
				literal, ok := value.Values[i].(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					return "", errors.New("Version must be a string constant")
				}
				version, err := strconv.Unquote(literal.Value)
				if err != nil {
					return "", err
				}
				if !semver.IsValid("v" + version) {
					return "", fmt.Errorf("invalid Version %q", version)
				}
				return version, nil
			}
		}
	}
	return "", errors.New("Version constant not found")
}

func selectBaseline(tags []string, current string, currentTagged bool) (baseline string, breaking bool, err error) {
	if !semver.IsValid(current) {
		return "", false, fmt.Errorf("invalid current version %q", current)
	}
	valid := make([]string, 0, len(tags))
	for _, tag := range tags {
		if semver.IsValid(tag) && (!currentTagged || tag != current) {
			valid = append(valid, tag)
		}
	}
	if len(valid) == 0 {
		return "", false, errors.New("no semantic-version tags found")
	}
	sort.Slice(valid, func(i, j int) bool { return semver.Compare(valid[i], valid[j]) > 0 })
	latest := valid[0]
	if semver.Compare(current, latest) <= 0 {
		return "", false, fmt.Errorf("current version %s is not newer than %s", current, latest)
	}
	if semver.Major(current) == "v0" && semver.Prerelease(current) != "" {
		hasStableSeries := false
		for _, tag := range valid {
			if compatibilitySeries(tag) == compatibilitySeries(current) && semver.Prerelease(tag) == "" {
				hasStableSeries = true
				break
			}
		}
		if !hasStableSeries {
			return latest, true, nil
		}
	}
	series := compatibilitySeries(current)
	for _, tag := range valid {
		if compatibilitySeries(tag) == series {
			return tag, false, nil
		}
	}
	return latest, true, nil
}

func compatibilitySeries(version string) string {
	major := semver.Major(version)
	if major != "v0" {
		return major
	}
	base := strings.TrimPrefix(semver.Canonical(version), "v")
	parts := strings.Split(base, ".")
	if len(parts) < 2 {
		return ""
	}
	return "v0." + parts[1]
}

func extractTag(root, tag, destination string) error {
	cmd := exec.Command("git", "archive", "--format=tar", tag)
	cmd.Dir = root
	archive, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("git archive %s: %w", tag, err)
	}
	reader := tar.NewReader(bytes.NewReader(archive))
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		path := filepath.Join(destination, filepath.FromSlash(header.Name))
		if !strings.HasPrefix(path, destination+string(filepath.Separator)) {
			return fmt.Errorf("archive path escapes destination: %q", header.Name)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			contents, err := io.ReadAll(reader)
			if err != nil {
				return err
			}
			if err := os.WriteFile(path, contents, os.FileMode(header.Mode)); err != nil {
				return err
			}
		}
	}
}

func loadPackage(dir, path string) (*packages.Package, error) {
	config := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
			packages.NeedImports | packages.NeedDeps | packages.NeedTypes | packages.NeedTypesInfo,
		Dir: dir,
		Env: append(os.Environ(), "GOWORK=off"),
	}
	loaded, err := packages.Load(config, path)
	if err != nil {
		return nil, err
	}
	if len(loaded) != 1 {
		return nil, fmt.Errorf("loaded %d packages", len(loaded))
	}
	if count := packages.PrintErrors(loaded); count != 0 {
		return nil, fmt.Errorf("package has %d load error(s)", count)
	}
	if loaded[0].Types == nil {
		return nil, errors.New("package has no type information")
	}
	return loaded[0], nil
}

func ignoredMetadataChange(path, message string) bool {
	var names []string
	switch path {
	case "github.com/airlockrun/agentsdk":
		names = []string{"Version", "HTMXVersion"}
	case "github.com/airlockrun/goai":
		names = []string{"Version"}
	default:
		return false
	}
	for _, name := range names {
		if strings.HasPrefix(message, name+": value changed from ") {
			return true
		}
	}
	return false
}

func commandOutput(dir, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return string(output), nil
}
