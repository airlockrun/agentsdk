//go:build windows

package main

import "os/exec"

func configureManifestProcess(*exec.Cmd) {}

func terminateManifestDescendants(*exec.Cmd) {}
