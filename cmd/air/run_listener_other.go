//go:build !linux && !darwin

package main

import (
	"errors"
	"net"
	"os/exec"
)

func inheritLocalListener(*exec.Cmd, net.Listener) (func() error, error) {
	return nil, errors.New("local app socket activation requires Linux or macOS")
}
