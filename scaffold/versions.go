package scaffold

// Build toolchain versions a scaffolded agent is pinned to. This is
// the single source of truth: the scaffold templates render from these consts
// (go.mod.tmpl's templ require, Dockerfile.tmpl's tailwind/daisyui ARGs), so a
// materialized agent can never drift from them. The airlock agent-builder
// image bakes the same toolchain at fixed paths for its iterative codegen loop;
// its Dockerfile ARGs are literals validated against these consts by
// airlock/scripts/check-versions.sh (Docker can't read a Go const, so the ARG
// stays a literal and the check enforces equality).
//
// HTMXVersion lives in the agentsdk root package (assets.go) because htmx ships
// as an embedded runtime asset, not a build-time fetch.
const (
	// TemplVersion pins the templ CLI and runtime library. The generator and
	// the linked library MUST match — generated *_templ.go calls runtime
	// symbols the library must expose.
	TemplVersion = "v0.3.1020"
	// TailwindVersion pins the standalone Tailwind v4 binary (Rust, no Node).
	TailwindVersion = "v4.1.0"
	// DaisyUIVersion pins the DaisyUI plugin (daisyui.mjs / daisyui-theme.mjs)
	// the standalone Tailwind binary loads by path.
	DaisyUIVersion = "v5.5.23"
	// SQLCVersion pins the standalone sqlc generator used when query files are
	// present. sqlc is not an agent module dependency.
	SQLCVersion = "1.30.0"
	// GoVersion is the toolchain version stamped into a scaffolded agent's
	// go.mod `go` directive and its Dockerfile `FROM golang:` tag. Three-
	// component form matches what `go mod tidy` rewrites the directive to on
	// Go 1.21+, so go.work/go.mod version checks don't trip.
	GoVersion = "1.26.0"
)
