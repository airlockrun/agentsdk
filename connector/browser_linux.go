//go:build linux

package connector

import "os/exec"

func openBrowser(target string) error { return exec.Command("xdg-open", target).Start() }
