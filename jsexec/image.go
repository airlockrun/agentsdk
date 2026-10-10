package jsexec

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

// PublishedExecutorImage is an immutable Airlock release artifact, not an SDK
// version-derived tag. Its compatibility is tied to the reviewed runtime below.
const PublishedExecutorImage = "ghcr.io/airlockrun/airlock-js-executor@sha256:460592372756ec350e30c3c7701481733b31368d42565eecc709bb9619090cd6"
const publishedCompatibleRuntime = "df404d86d21499d01a0fd1824027420f89083ccf09c705631771ea98b8f62157"
const publishedProtocolSHA = "9c07d6c5ecdb1cd926a5d2104165734fe254c1dc4dc4e5590cb34d4470b7a337"

// RuntimeFingerprint covers the trusted, dependency-free Docker supervisor and
// scripts plus the pinned Deno runtime, independently of SDK semver.
func RuntimeFingerprint() string {
	h := sha256.New()
	h.Write([]byte(DenoImage + "\x00"))
	for _, name := range []string{"session.go", "protocol.go", "supervisor.go", "runtime/controller.mjs", "runtime/worker.mjs"} {
		data, err := RuntimeAssets.ReadFile(name)
		if err != nil {
			panic(err)
		}
		h.Write([]byte(name + "\x00"))
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil))
}

type imageMetadata struct {
	ID, Os, Architecture string
	RepoDigests          []string
	Config               struct{ Labels map[string]string }
}

// PrepareDockerImage validates or pulls an explicitly selected image. Newly
// built images carry the full runtime fingerprint. The reviewed public artifact
// also requires exact protocol/Deno metadata, scripts and a framed smoke call.
func PrepareDockerImage(ctx context.Context, image string) (string, error) {
	if image == "" || strings.HasPrefix(image, "-") {
		return "", errors.New("jsexec: explicit Docker executor image is required")
	}
	if out, err := exec.CommandContext(ctx, "docker", "info", "--format", "{{.ServerVersion}}").CombinedOutput(); err != nil {
		return "", fmt.Errorf("jsexec: Docker executor requires a running daemon: %w: %s", err, out)
	}
	inspect := func() ([]byte, error) { return exec.CommandContext(ctx, "docker", "image", "inspect", image).Output() }
	raw, err := inspect()
	if err != nil {
		if out, err := exec.CommandContext(ctx, "docker", "pull", image).CombinedOutput(); err != nil {
			return "", fmt.Errorf("jsexec: obtain executor image; configure a compatible image or use --build-executor: %w: %s", err, out)
		}
		raw, err = inspect()
		if err != nil {
			return "", err
		}
	}
	var images []imageMetadata
	if err := json.Unmarshal(raw, &images); err != nil || len(images) != 1 {
		return "", errors.New("jsexec: invalid executor image metadata")
	}
	meta := images[0]
	if meta.Os != "linux" {
		return "", errors.New("jsexec: executor image must target Linux")
	}
	if meta.Architecture != runtime.GOARCH {
		return "", errors.New("jsexec: executor image architecture differs from this local SDK runtime; select a compatible image or use --build-executor")
	}
	if meta.Config.Labels["run.airlock.executor.runtime-source"] == RuntimeFingerprint() && meta.Config.Labels["run.airlock.executor.deno-image"] == DenoImage {
		return meta.ID, nil
	}
	reviewed := false
	for _, digest := range meta.RepoDigests {
		if digest == PublishedExecutorImage {
			reviewed = true
		}
	}
	if !reviewed || RuntimeFingerprint() != publishedCompatibleRuntime || meta.Config.Labels["run.airlock.executor.protocol-source"] != publishedProtocolSHA || meta.Config.Labels["run.airlock.executor.deno-image"] != DenoImage {
		return "", errors.New("jsexec: executor image runtime is not compatible with this SDK's embedded assets; select a compatible image or use --build-executor")
	}
	out, err := exec.CommandContext(ctx, "docker", "run", "--rm", "--network", "none", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--entrypoint", "sha256sum", meta.ID, "/opt/jsexec/controller.mjs", "/opt/jsexec/worker.mjs").Output()
	if err != nil {
		return "", fmt.Errorf("jsexec: inspect published runtime scripts: %w", err)
	}
	fields := strings.Fields(string(out))
	if len(fields) != 4 {
		return "", errors.New("jsexec: invalid published runtime checksums")
	}
	for i, name := range []string{"controller.mjs", "worker.mjs"} {
		data, err := RuntimeAssets.ReadFile("runtime/" + name)
		if err != nil {
			return "", err
		}
		hash := sha256.Sum256(data)
		if fields[i*2] != hex.EncodeToString(hash[:]) {
			return "", errors.New("jsexec: published executor runtime script mismatch; use --build-executor")
		}
	}
	session, err := NewDockerSession(ctx, meta.ID, Options{Limits: DefaultLimits()})
	if err != nil {
		return "", err
	}
	result, runErr := session.Execute(ctx, "return 6*7;", InvokerFunc(func(context.Context, string, json.RawMessage) (json.RawMessage, error) {
		return nil, errors.New("unexpected compatibility callback")
	}))
	closeErr := session.Close()
	if err := errors.Join(runErr, closeErr); err != nil {
		return "", fmt.Errorf("jsexec: published executor protocol smoke: %w", err)
	}
	if string(result.Output) != "42" {
		return "", errors.New("jsexec: published executor protocol smoke returned invalid output")
	}
	return meta.ID, nil
}
