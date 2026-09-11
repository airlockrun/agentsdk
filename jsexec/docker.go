package jsexec

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const executorMainSource = `package main
import "github.com/airlockrun/agentsdk/jsexec"
func main() { jsexec.Main() }
`

// BuildImage builds the shared executor recipe using embedded sources and the
// caller's installed Go toolchain and Docker CLI. It needs no Airlock release,
// module dependencies, credentials in the build context, or source checkout.
// The output is Linux for the calling process's architecture.
func BuildImage(ctx context.Context, tag string) error {
	if tag == "" || strings.HasPrefix(tag, "-") {
		return errors.New("jsexec: image tag required")
	}
	dir, err := os.MkdirTemp("", "jsexec-build-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err := os.Mkdir(filepath.Join(dir, "jsexec"), 0700); err != nil {
		return err
	}
	files := map[string][]byte{"go.mod": []byte("module github.com/airlockrun/agentsdk\n\ngo 1.26.0\n"), "main.go": []byte(executorMainSource)}
	for _, name := range []string{"session.go", "protocol.go", "supervisor.go"} {
		b, err := RuntimeAssets.ReadFile(name)
		if err != nil {
			return err
		}
		files["jsexec/"+name] = b
	}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0600); err != nil {
			return err
		}
	}
	binary := filepath.Join(dir, "jsexecutor")
	build := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", binary, ".")
	build.Dir = dir
	build.Env = append(os.Environ(), "GOWORK=off", "GOTOOLCHAIN=local", "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	if out, err := build.CombinedOutput(); err != nil {
		return fmt.Errorf("jsexec: build supervisor: %w: %s", err, out)
	}
	var archive bytes.Buffer
	tw := tar.NewWriter(&archive)
	for _, name := range []string{"Dockerfile", "controller.mjs", "worker.mjs", "jsexecutor"} {
		var b []byte
		var err error
		if name == "jsexecutor" {
			b, err = os.ReadFile(binary)
		} else {
			b, err = RuntimeAssets.ReadFile("runtime/" + name)
		}
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0755, Size: int64(len(b))}); err != nil {
			return err
		}
		if _, err := tw.Write(b); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "docker", "build", "--network", "none", "-t", tag, "-")
	cmd.Stdin = &archive
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("jsexec: build image: %w: %s", err, out)
	}
	return nil
}

// DockerSession owns a disposable, credential-free executor container. Its
// state cannot migrate between replicas; a lost attach/session is terminal.
type DockerSession struct {
	*Client
	ContainerID string
}

// NewDockerSession creates and attaches a container using the same production
// image built by BuildImage. ctx owns its lifetime, not just startup.
func NewDockerSession(ctx context.Context, image string, options Options) (*DockerSession, error) {
	if image == "" || strings.HasPrefix(image, "-") {
		return nil, errors.New("jsexec: image required")
	}
	if err := options.validate(); err != nil {
		return nil, err
	}
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, err
	}
	name := "jsexec-" + hex.EncodeToString(random[:])
	create := exec.CommandContext(ctx, "docker", "create", "--name", name, "--interactive", "--network", "none", "--memory", "256m", "--memory-swap", "256m", "--cpus", "1", "--pids-limit", "128", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--read-only", "--user", "65532:65532", "--tmpfs", "/tmp:rw,noexec,nosuid,size=32m", "--log-driver", "none", image)
	out, err := create.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("jsexec: create container: %w: %s", err, out)
	}
	transport := &dockerTransport{id: strings.TrimSpace(string(out))}
	cmd := exec.Command("docker", "start", "--attach", "--interactive", transport.id)
	transport.cmd = cmd
	transport.in, err = cmd.StdinPipe()
	if err != nil {
		_ = transport.Close()
		return nil, err
	}
	transport.out, err = cmd.StdoutPipe()
	if err != nil {
		_ = transport.Close()
		return nil, err
	}
	// Container stderr cannot enter the framed stdout stream or grow a buffer.
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		_ = transport.Close()
		return nil, err
	}
	client, err := NewClient(transport, options)
	if err != nil {
		_ = transport.Close()
		return nil, err
	}
	client.mu.Lock()
	client.stop = context.AfterFunc(ctx, func() { _ = client.Close() })
	client.mu.Unlock()
	return &DockerSession{Client: client, ContainerID: transport.id}, nil
}

type dockerTransport struct {
	id   string
	cmd  *exec.Cmd
	in   io.WriteCloser
	out  io.ReadCloser
	once sync.Once
}

func (d *dockerTransport) Read(p []byte) (int, error)  { return d.out.Read(p) }
func (d *dockerTransport) Write(p []byte) (int, error) { return d.in.Write(p) }
func (d *dockerTransport) Close() error {
	d.once.Do(func() {
		if d.in != nil {
			_ = d.in.Close()
		}
		if d.out != nil {
			_ = d.out.Close()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = exec.CommandContext(ctx, "docker", "rm", "--force", d.id).Run()
		if d.cmd != nil && d.cmd.Process != nil {
			_ = d.cmd.Process.Kill()
			_ = d.cmd.Wait()
		}
	})
	return nil
}
