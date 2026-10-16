//go:build airlockplatformintegration

package jsexec

import "testing"

func TestPublishedExecutorCompatibility(t *testing.T) {
	if _, err := PrepareDockerImage(t.Context(), PublishedExecutorImage); err != nil {
		t.Fatal(err)
	}
}
func TestDockerImageCompatibilityRejectsUnknownRuntime(t *testing.T) {
	if _, err := PrepareDockerImage(t.Context(), DenoImage); err == nil {
		t.Fatal("stock Deno accepted as supervised executor")
	}
}
func TestBuiltExecutorCompatibility(t *testing.T) {
	const image = "agentsdk-jsexec-compatibility:fixture"
	if err := BuildImage(t.Context(), image); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareDockerImage(t.Context(), image); err != nil {
		t.Fatal(err)
	}
}
