//go:build windows

package connector

import "os/exec"

func openBrowser(target string) error {
	return exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", target).Start()
}
