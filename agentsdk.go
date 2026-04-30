// Package agentsdk provides the Go SDK for building Airlock agents.
//
// This package will contain the client library that agents use to communicate
// with the Airlock platform — registering connections, receiving triggers,
// and streaming run results.
package agentsdk

// Version is the agentsdk API version. Reported to Airlock during sync.
// Bump on breaking changes — see CLAUDE.md for versioning rules.
const Version = "0.2.0" // pre-release; bump when shipping breaking protocol changes
