package agentsdk

import (
	"net"
	"strconv"
	"testing"
)

func TestRuntimeListenerActivation(t *testing.T) {
	original, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	file, err := original.File()
	if err != nil {
		t.Skipf("socket activation unsupported: %v", err)
	}
	defer file.Close()
	t.Setenv("AIRLOCK_LOCAL_RUNTIME", "1")
	t.Setenv("AIRLOCK_LOCAL_LISTEN_FD", strconv.Itoa(int(file.Fd())))
	inherited, err := runtimeListener("ignored:0")
	if err != nil {
		t.Fatal(err)
	}
	defer inherited.Close()
	address := original.Addr().String()
	original.Close()
	if inherited.Addr().String() != address {
		t.Fatal("inherited socket changed address")
	}
	if stolen, err := net.Listen("tcp", address); err == nil {
		stolen.Close()
		t.Fatal("activated address was released")
	}
}

func TestRuntimeListenerRequiresExplicitActivation(t *testing.T) {
	t.Setenv("AIRLOCK_LOCAL_LISTEN_FD", "3")
	t.Setenv("AIRLOCK_LOCAL_RUNTIME", "")
	if _, err := runtimeListener("127.0.0.1:0"); err == nil {
		t.Fatal("unselected activation accepted")
	}
}
