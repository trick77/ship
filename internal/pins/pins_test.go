package pins

import (
	"os"
	"strings"
	"testing"
)

// cosign's version is cosign-installer's default: Dependabot moves the installer, a cosign-release pin it never reads.
func TestCosign_whenReleaseWorkflow_thenInstallerDefault(t *testing.T) {
	b, err := os.ReadFile("../../.github/workflows/release.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "cosign-release:") {
		t.Fatal("release.yaml pins cosign-release: Dependabot cannot bump it, drop it for the installer's default")
	}
}
