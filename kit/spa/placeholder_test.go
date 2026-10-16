package spa

import (
	"strings"
	"testing"
)

// The default page serves every repo: it names no build command, which most repos would not have.
func TestPlaceholder_namesNoBuildCommand(t *testing.T) {
	page := string(defaultPlaceholder)
	if strings.Contains(page, "make ") || strings.Contains(page, "npm ") {
		t.Errorf("placeholder names a build command:\n%s", page)
	}
	if !strings.Contains(page, "released image") {
		t.Errorf("placeholder lost the released-image hint:\n%s", page)
	}
}
