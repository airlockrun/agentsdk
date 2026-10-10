package jsexec

import (
	"os"
	"testing"
)

func TestRuntimeFingerprint(t *testing.T) {
	if RuntimeFingerprint() != publishedCompatibleRuntime {
		t.Fatal("embedded Docker runtime differs from the reviewed public artifact; update compatibility only after validation")
	}
}
func TestPublishedExecutorCompatibility(t *testing.T) {
	if os.Getenv("JSEXEC_DOCKER_TEST") != "1" {
		t.Skip("set JSEXEC_DOCKER_TEST=1 for published artifact validation")
	}
	if _, err := PrepareDockerImage(t.Context(), PublishedExecutorImage); err != nil {
		t.Fatal(err)
	}
}

func TestDockerImageCompatibilityRejectsUnknownRuntime(t *testing.T) {
	if os.Getenv("JSEXEC_DOCKER_TEST") != "1" {
		t.Skip("set JSEXEC_DOCKER_TEST=1 for image identity validation")
	}
	if _, err := PrepareDockerImage(t.Context(), DenoImage); err == nil {
		t.Fatal("stock Deno image was accepted as the supervised executor")
	}
}

func TestBuiltExecutorCompatibility(t *testing.T) {
	if os.Getenv("JSEXEC_DOCKER_TEST") != "1" {
		t.Skip("set JSEXEC_DOCKER_TEST=1 for embedded recipe validation")
	}
	const image = "agentsdk-jsexec-compatibility:fixture"
	if err := BuildImage(t.Context(), image); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareDockerImage(t.Context(), image); err != nil {
		t.Fatal(err)
	}
}
