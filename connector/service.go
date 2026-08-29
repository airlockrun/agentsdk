package connector

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/airlockrun/agentsdk/connector/protocol"
)

type ServiceMode string

const (
	ServiceSystem ServiceMode = "system"
	ServiceUser   ServiceMode = "user"
)

// Operations isolates process execution for lifecycle tests.
type Operations interface {
	Execute(context.Context, string, ...string) ([]byte, error)
	Executable() (string, error)
}

type systemOperations struct{}

func (systemOperations) Execute(ctx context.Context, name string, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, args...)
	body, err := command.CombinedOutput()
	if err != nil {
		return body, fmt.Errorf("%s %v: %w: %s", name, args, err, body)
	}
	return body, nil
}

func (systemOperations) Executable() (string, error) { return os.Executable() }

func executableDigest(operations Operations) (string, error) {
	path, err := operations.Executable()
	if err != nil {
		return "", err
	}
	return fileDigest(path)
}

func fileDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	if err := protocol.ValidateArtifactDigest(digest); err != nil {
		return "", err
	}
	return digest, nil
}

type serviceManager interface {
	PrepareIdentity(context.Context) error
	ValidateIdentity(context.Context, string, string) error
	Install(context.Context) error
	Uninstall(context.Context) error
	Start(context.Context) error
	Stop(context.Context) error
	Status(context.Context) (string, error)
	Reconfigure(context.Context) (func() error, error)
	Upgrade(context.Context, func() error) (bool, error)
	Enable(context.Context) error
	Disable(context.Context) error
	Installed() bool
	Rollback(context.Context) error
	RollbackDigest() (string, error)
}
