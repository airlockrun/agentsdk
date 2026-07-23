// Package agentsdk provides the Go SDK for building Airlock agents.
//
// Agents register typed tools, routes, webhooks, schedules, connections, and
// other capabilities, then serve them through the Airlock runtime. See the
// repository README and REFERENCE.md for the project guide and complete API reference.
package agentsdk

// Version is the agentsdk API version. Reported to Airlock during sync.
// Bump on breaking changes — see AGENTS.md for versioning rules. Pre-commit
// gate enforces Version > latest git tag in this repo.
const Version = "0.4.0-rc.33"
