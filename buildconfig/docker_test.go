package buildconfig

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestRuntimeFilesDockerRecipe(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping Docker integration test in short mode")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is unavailable")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker daemon is unavailable")
	}

	contextDir := t.TempDir()
	air := exec.Command("go", "build", "-o", filepath.Join(contextDir, "air"), "../cmd/air")
	air.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := air.CombinedOutput(); err != nil {
		t.Fatalf("build air: %v\n%s", err, out)
	}
	writeFile(t, contextDir, "airlock.toml", "[build]\nruntime_files = [\"python-app/\"]\n", 0o644)
	writeFile(t, contextDir, ".gitignore", "python-app/secret.env\n", 0o644)
	writeFile(t, contextDir, "python-app/start.sh", "#!/bin/sh\n", 0o755)
	writeFile(t, contextDir, "python-app/secret.env", "SECRET=bad\n", 0o600)
	writeFile(t, contextDir, "Dockerfile", `FROM scratch AS builder
COPY air /air
COPY . /build
RUN ["/air", "stage-runtime-files", "/build", "/runtime-files"]
FROM scratch
COPY --from=builder /runtime-files/ /app/
CMD ["/app/python-app/start.sh"]
`, 0o644)

	tag := fmt.Sprintf("airlock-runtime-files-test:%d", time.Now().UnixNano())
	build := exec.Command("docker", "build", "-t", tag, contextDir)
	build.Env = append(os.Environ(), "DOCKER_BUILDKIT=1")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("docker build: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rmi", tag).Run() })

	containerOut, err := exec.Command("docker", "create", tag).CombinedOutput()
	if err != nil {
		t.Fatalf("docker create: %v\n%s", err, containerOut)
	}
	containerID := strings.TrimSpace(string(containerOut))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", containerID).Run() })
	var exported bytes.Buffer
	var exportErr bytes.Buffer
	export := exec.Command("docker", "export", containerID)
	export.Stdout = &exported
	export.Stderr = &exportErr
	if err := export.Run(); err != nil {
		t.Fatalf("docker export: %v\n%s", err, exportErr.Bytes())
	}

	files := map[string]*tar.Header{}
	tr := tar.NewReader(&exported)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		copy := *hdr
		files[strings.TrimPrefix(hdr.Name, "/")] = &copy
	}
	start, ok := files["app/python-app/start.sh"]
	if !ok {
		t.Fatalf("runtime image files = %v", mapKeys(files))
	}
	if start.Mode&0o111 == 0 {
		t.Fatalf("start.sh mode = %o, want executable", start.Mode)
	}
	if _, ok := files["app/python-app/secret.env"]; ok {
		t.Fatal("ignored secret was copied into the runtime image")
	}
}

func mapKeys(values map[string]*tar.Header) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
