package agentsdk

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
)

// runtimeListener accepts the local CLI's already-bound socket. File activation
// keeps the private app port reserved continuously through process startup.
func runtimeListener(addr string) (net.Listener, error) {
	value := os.Getenv("AIRLOCK_LOCAL_LISTEN_FD")
	if value == "" {
		return net.Listen("tcp", addr)
	}
	if os.Getenv("AIRLOCK_LOCAL_RUNTIME") != "1" {
		return nil, errors.New("agentsdk: local listener activation requires AIRLOCK_LOCAL_RUNTIME=1")
	}
	fd, err := strconv.Atoi(value)
	if err != nil || fd < 3 {
		return nil, errors.New("agentsdk: local listener activation requires a valid inherited file descriptor")
	}
	file := os.NewFile(uintptr(fd), "airlock-local-listener")
	if file == nil {
		return nil, errors.New("agentsdk: missing inherited local listener")
	}
	defer file.Close()
	listener, err := net.FileListener(file)
	if err != nil {
		return nil, fmt.Errorf("agentsdk: inherit local listener: %w", err)
	}
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok || !address.IP.IsLoopback() {
		listener.Close()
		return nil, errors.New("agentsdk: inherited local listener must be loopback TCP")
	}
	return listener, nil
}
