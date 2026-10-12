package pins

import (
	"os"
	"strings"
	"testing"
)

// The Release workflow installs cosign itself (cosign-installer): its pin must be this one.
func TestCosign_whenReleaseWorkflow_thenSamePin(t *testing.T) {
	b, err := os.ReadFile("../../.github/workflows/release.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "cosign-release: "+Cosign+"\n") {
		t.Fatalf("release.yaml's cosign-release is not pins.Cosign %s", Cosign)
	}
}
