//go:build darwin

package connector

import "os/exec"

func openBrowser(target string) error { return exec.Command("/usr/bin/open", target).Start() }
