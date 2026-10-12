package config

import "testing"

func TestParse_whenAllowAndVisibility_thenMatched(t *testing.T) {
	c, err := Parse([]byte("visibility: private\nallow:\n  - { rule: distroless, image: -agent, why: smartctl needs root }\n" +
		"  - { rule: scaffold, path: ui/eslint.config.js, why: own rule }\n  - { rule: golangci, why: legacy }\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Visibility != "private" {
		t.Errorf("visibility %q", c.Visibility)
	}
	agent, own := "-agent", ""
	if a, ok := c.Allowed("distroless", &agent, "Containerfile"); !ok || a.Why != "smartctl needs root" {
		t.Errorf("agent: %+v %v", a, ok)
	}
	if _, ok := c.Allowed("distroless", &own, "Containerfile"); ok {
		t.Error("own image allowed")
	}
	if _, ok := c.Allowed("distroless", nil, "Containerfile"); ok {
		t.Error("no image allowed by an image entry")
	}
	if _, ok := c.Allowed("scaffold", nil, "Makefile"); ok {
		t.Error("other path allowed")
	}
	if _, ok := c.Allowed("scaffold", nil, "ui/eslint.config.js"); !ok {
		t.Error("path entry")
	}
	if _, ok := c.Allowed("golangci", nil, "x/.golangci.yaml"); !ok {
		t.Error("rule-wide entry")
	}
}

func TestParse_whenAllowWithoutWhyOrBadVisibility_thenError(t *testing.T) {
	for _, s := range []string{"allow:\n  - { rule: scaffold }\n", "allow:\n  - { why: x }\n", "visibility: internal\n"} {
		if _, err := Parse([]byte(s)); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
}
