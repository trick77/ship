package spa

import (
	"strings"
	"testing"
)

// The default page serves every repo: it names no build command, which most repos would not have. Its only code
// span is dist/.
func TestPlaceholder_namesNoBuildCommand(t *testing.T) {
	page := string(defaultPlaceholder)
	if strings.Count(page, "<code>") != 1 || !strings.Contains(page, "<code>dist/</code>") {
		t.Errorf("placeholder names something beyond dist/:\n%s", page)
	}
	if !strings.Contains(page, "released image") {
		t.Errorf("placeholder lost the released-image hint:\n%s", page)
	}
}
