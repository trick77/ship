package config

import (
	"slices"
	"testing"
)

func TestParse_whenDependabotIgnore_thenListed(t *testing.T) {
	c, err := Parse([]byte("dependabot:\n  ignore:\n" +
		"    - { ecosystem: gomod, dependency: github.com/ncruces/go-sqlite3, why: bindings welded to v0.23 }\n" +
		"    - { ecosystem: npm, dependency: mermaid, versions: [\">=12.1.0\"], why: drops a diagram type }\n"))
	if err != nil {
		t.Fatal(err)
	}
	ig := c.Dependabot.Ignore
	if len(ig) != 2 || ig[0].Ecosystem != "gomod" || ig[0].Versions != nil ||
		!slices.Equal(ig[1].Versions, []string{">=12.1.0"}) || ig[1].Why != "drops a diagram type" {
		t.Errorf("ignore %+v", ig)
	}
}

func TestParse_whenDependabotIgnoreIncomplete_thenError(t *testing.T) {
	for _, s := range []string{
		"dependabot:\n  ignore:\n    - { ecosystem: gomod, dependency: x }\n",
		"dependabot:\n  ignore:\n    - { ecosystem: gomod, why: y }\n",
		"dependabot:\n  ignore:\n    - { ecosystem: pip, dependency: x, why: y }\n",
		"dependabot:\n  ignore:\n    - { dependency: x, why: y }\n",
		"dependabot:\n  ignore:\n    - { ecosystem: gomod, dependency: x, why: \"two\\nlines\" }\n",
		"dependabot:\n  ignore:\n    - { ecosystem: gomod, dependency: x, versions: [\"\"], why: y }\n",
	} {
		if _, err := Parse([]byte(s)); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
}
