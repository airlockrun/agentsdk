//go:build !linux && !windows && !darwin

package connector

import "errors"

func openBrowser(string) error {
	return errors.New("connector: browser activation is unsupported on this platform; use --no-browser")
}
