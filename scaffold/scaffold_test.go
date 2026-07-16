package scaffold

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMaterialize(t *testing.T) {
	dir := t.TempDir()

	data := ScaffoldData{
		AgentID:         "550e8400-e29b-41d4-a716-446655440000",
		GoVersion:       "1.26",
		AgentSDKVersion: "v1.0.0",
		AgentBaseImage:  "airlock-agent-base",
	}

	if err := Materialize(dir, data); err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	// Verify all expected files exist
	expectedFiles := []string{
		"main.go",
		"main_test.go",
		"handlers/home_test.go",
		"NOTES.md",
		"go.mod",
		"sqlc.yaml",
		"internal/db/doc.go",
		"Dockerfile",
		".gitignore",
		"THIRD_PARTY_NOTICES.generated.md",
	}
	if _, err := os.Stat(filepath.Join(dir, "deps", "deps.go")); !os.IsNotExist(err) {
		t.Errorf("deps/deps.go must not be generated: %v", err)
	}
	for _, f := range expectedFiles {
		path := filepath.Join(dir, f)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("expected file %s not found: %v", f, err)
		}
	}

	// Verify empty directories exist
	expectedDirs := []string{
		"db/migrations",
		"db/queries",
	}
	for _, d := range expectedDirs {
		path := filepath.Join(dir, d)
		info, err := os.Stat(path)
		if err != nil {
			t.Errorf("expected dir %s not found: %v", d, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("%s is not a directory", d)
		}
	}

	// Verify main.go content
	mainGo, err := os.ReadFile(filepath.Join(dir, "main.go"))
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	if !strings.Contains(string(mainGo), "agentsdk.New(agentsdk.Config{") {
		t.Error("main.go missing agentsdk.New(agentsdk.Config{)")
	}
	if !strings.Contains(string(mainGo), "func newAgent()") {
		t.Error("main.go missing func newAgent() — tests build the agent through it")
	}
	if !strings.Contains(string(mainGo), "newAgent().Serve()") {
		t.Error("main.go missing newAgent().Serve()")
	}
	if !strings.Contains(string(mainGo), "pages := handlers.New()") {
		t.Error("main.go missing handler construction")
	}
	if !strings.Contains(string(mainGo), "Handler:     pages.Home") {
		t.Error("main.go missing bound home handler registration")
	}
	if !strings.Contains(string(mainGo), "agent.RegisterStaticAsset(&agentsdk.StaticAsset{") {
		t.Error("main.go missing SDK static asset registration")
	}

	mainTest, err := os.ReadFile(filepath.Join(dir, "main_test.go"))
	if err != nil {
		t.Fatalf("read main_test.go: %v", err)
	}
	if !strings.Contains(string(mainTest), "agenttest.WithUser") {
		t.Error("main_test.go missing authenticated caller helper")
	}

	homeGo, err := os.ReadFile(filepath.Join(dir, "handlers", "home.go"))
	if err != nil {
		t.Fatalf("read handlers/home.go: %v", err)
	}
	if !strings.Contains(string(homeGo), "func New() *Handler") || !strings.Contains(string(homeGo), "func (h *Handler) Home") {
		t.Error("handlers/home.go missing constructed receiver handler")
	}

	notes, err := os.ReadFile(filepath.Join(dir, "NOTES.md"))
	if err != nil {
		t.Fatalf("read NOTES.md: %v", err)
	}
	for _, heading := range []string{"## Product", "## Architecture", "## Integrations", "## UI"} {
		if !strings.Contains(string(notes), heading) {
			t.Errorf("NOTES.md missing %q heading", heading)
		}
	}

	// Committed go.mod has no /libs/... replace block — those live only
	// in the build-time go.work that airlock injects into codegen and
	// docker contexts. Keeping replaces out of the committed file lets
	// user clones compile against public modules.
	goMod, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	goModStr := string(goMod)
	if !strings.Contains(goModStr, "module agent") {
		t.Error("go.mod missing managed module path")
	}
	for _, unwanted := range []string{"/libs/agentsdk", "/libs/goai", "/libs/sol", "/libs/goose", "/libs/templ"} {
		if strings.Contains(goModStr, unwanted) {
			t.Errorf("go.mod must not contain %s replace directive (build-time go.work supplies it)", unwanted)
		}
	}
	if !strings.Contains(goModStr, "agentsdk v1.0.0") {
		t.Errorf("go.mod should pin agentsdk to AgentSDKVersion (v1.0.0); got:\n%s", goModStr)
	}
	if !strings.Contains(goModStr, "tool github.com/airlockrun/agentsdk/cmd/air") {
		t.Errorf("go.mod should expose air as a module-local tool; got:\n%s", goModStr)
	}
	if !strings.Contains(goModStr, "tool github.com/a-h/templ/cmd/templ") {
		t.Errorf("go.mod should expose templ as a module-local tool; got:\n%s", goModStr)
	}

	// .gitignore lists the build-time-only files airlock generates
	// (go.work pair). Dockerfile is committed so users can build the
	// container locally; airlock-side builds regenerate it into a temp
	// dir and use `docker build -f` to avoid touching the committed copy.
	gitignore, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	for _, want := range append([]string{"go.work", "go.work.sum", ".airlock/local/", ".airlock/toolchain/"}, GeneratedArtifactIgnorePatterns()...) {
		if !strings.Contains(string(gitignore), want) {
			t.Errorf(".gitignore missing %q entry", want)
		}
	}
	if strings.Contains(string(gitignore), "Dockerfile") {
		t.Error(".gitignore must not list Dockerfile (committed for local-build support)")
	}

	// Dockerfile uses the goproxy build-context stage for lib resolution
	// (airlock overrides it in dev; empty by default → public proxy) and
	// the agent-base runtime + dep hooks.
	if err := GenerateDockerfile(dir, data); err != nil {
		t.Fatalf("GenerateDockerfile: %v", err)
	}
	dockerfile, err := os.ReadFile(filepath.Join(dir, "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	dockerfileStr := string(dockerfile)
	if !strings.Contains(dockerfileStr, "golang:1.26") {
		t.Error("Dockerfile missing golang version")
	}
	if !strings.Contains(dockerfileStr, "airlock-agent-base") {
		t.Error("Dockerfile missing agent base image")
	}
	if !strings.Contains(dockerfileStr, "FROM scratch AS goproxy") {
		t.Error("Dockerfile missing the goproxy build-context stage")
	}
	if !strings.Contains(dockerfileStr, "GOPROXY=") {
		t.Error("Dockerfile missing GOPROXY in the build RUN")
	}
	if !strings.Contains(dockerfileStr, "FROM sqlc/sqlc:1.30.0 AS sqlc") || !strings.Contains(dockerfileStr, ".airlock/toolchain/bin/sqlc generate") {
		t.Error("Dockerfile missing pinned conditional sqlc generation")
	}
	if strings.Contains(dockerfileStr, "--from=libs") {
		t.Error("Dockerfile must not reference the old libs-owned/libs-ext contexts")
	}
	if !strings.Contains(dockerfileStr, "setup.sh") {
		t.Error("Dockerfile missing setup.sh hook")
	}
	if !strings.Contains(dockerfileStr, "ARG SQLC_VERSION=1.30.0") || !strings.Contains(dockerfileStr, "FROM sqlc/sqlc:${SQLC_VERSION} AS sqlc") {
		t.Error("Dockerfile missing pinned sqlc stage")
	}
	if !strings.Contains(dockerfileStr, ".airlock/toolchain/bin/sqlc generate") {
		t.Error("Dockerfile missing sqlc generation")
	}
	if !strings.Contains(dockerfileStr, "type=cache,target=/var/lib/apt/lists") {
		t.Error("Dockerfile missing apt cache mount")
	}
}

func TestMaterialize_RequiresSDKVersion(t *testing.T) {
	dir := t.TempDir()

	// Missing AgentSDKVersion → fail loud (go.mod would otherwise render
	// with an empty version and produce invalid Go module syntax).
	err := Materialize(dir, ScaffoldData{
		AgentID:   "550e8400-e29b-41d4-a716-446655440000",
		GoVersion: "1.26",
	})
	if err == nil {
		t.Fatal("expected error when AgentSDKVersion is empty")
	}
	if !strings.Contains(err.Error(), "AgentSDKVersion") {
		t.Fatalf("error = %v, want mention of AgentSDKVersion", err)
	}
}

func TestPackageLocalDepsCompositionCompiles(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"go.mod": "module agent\n\ngo 1.26\n",
		"feature/service.go": `package feature

type Service struct{ Prefix string }

func NewService(prefix string) *Service {
	if prefix == "" {
		panic("feature: prefix is required")
	}
	return &Service{Prefix: prefix}
}

type Deps struct {
	Service *Service
}

type Tools struct {
	service *Service
}

func NewTools(d Deps) *Tools {
	if d.Service == nil {
		panic("feature: service is required")
	}
	return &Tools{service: d.Service}
}

func (t *Tools) Label(name string) string { return t.service.Prefix + name }
`,
		"handlers/home.go": `package handlers

import "agent/feature"

type Deps struct {
	Feature *feature.Service
}

type Handler struct {
	feature *feature.Service
}

func New(d Deps) *Handler {
	if d.Feature == nil {
		panic("handlers: feature service is required")
	}
	return &Handler{feature: d.Feature}
}

func (h *Handler) Title() string { return h.feature.Prefix + "home" }
`,
		"main.go": `package main

import (
	"agent/feature"
	"agent/handlers"
)

func main() {
	service := feature.NewService("agent: ")
	tools := feature.NewTools(feature.Deps{Service: service})
	pages := handlers.New(handlers.Deps{Feature: service})
	_, _ = tools.Label("tool"), pages.Title()
}
`,
	}
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", name, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	cmd := exec.Command("go", "test", "./...")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("package-local dependency composition failed to compile: %v\n%s", err, out)
	}
}

func TestInstallSkills(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "skills")
	if err := os.MkdirAll(filepath.Join(dir, "removed"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "removed", "stale.md"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := InstallSkills(dir); err != nil {
		t.Fatalf("InstallSkills: %v", err)
	}
	for _, path := range []string{
		"manifest.json",
		"daisyui/SKILL.md",
		"htmx/reference/docs.md",
		"templ/reference/03-syntax-and-usage/06-if-else.md",
	} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(path))); err != nil {
			t.Errorf("installed skill file %s: %v", path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "removed", "stale.md")); !os.IsNotExist(err) {
		t.Fatalf("stale skill file remains after replacement: %v", err)
	}
	if SkillsDigest() == "" {
		t.Fatal("SkillsDigest is empty")
	}
}
