package jsexec

import "embed"

// DenoImage pins the registry manifest list for the stock Deno 2.9.6 image.
// Verified with docker pull and docker image inspect; see README.md.
const DenoImage = "denoland/deno@sha256:2014dc167ece617ef7e7ba40631ac2234c59e75ce693e7cc2dc2602b3c87859d"

// ProbeAssets contains a diagnostic image recipe and the hostile loader probes.
// These assets are not a production executor or a security boundary.
//
//go:embed probe/*
var ProbeAssets embed.FS

// RuntimeAssets contains the production/test Docker recipe, Deno assets, and
// dependency-free supervisor sources used by BuildImage.
//
//go:embed runtime/* session.go protocol.go supervisor.go
var RuntimeAssets embed.FS
