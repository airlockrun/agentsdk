//go:build linux || darwin

package main

import (
	"errors"
	"net"
	"os"
	"os/exec"
)

func inheritLocalListener(cmd *exec.Cmd, listener net.Listener) (func() error, error) {
	tcp, ok := listener.(*net.TCPListener)
	if !ok {
		return nil, errors.New("local app listener must be TCP")
	}
	file, err := tcp.File()
	if err != nil {
		return nil, err
	}
	cmd.ExtraFiles = []*os.File{file}
	cmd.Env = append(cmd.Env, "AIRLOCK_LOCAL_RUNTIME=1", "AIRLOCK_LOCAL_LISTEN_FD=3")
	return file.Close, nil
}
