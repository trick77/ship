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
	if _, ok := c.Allowed("scaffold", nil, "./ui//eslint.config.js"); !ok {
		t.Error("unclean path")
	}
	if _, ok := c.Allowed("scaffold", nil, ""); ok {
		t.Error("path entry matched no path")
	}
	c.Allow[1].Path = "./ui/eslint.config.js"
	if _, ok := c.Allowed("scaffold", nil, "ui/eslint.config.js"); !ok {
		t.Error("unclean entry path")
	}
	if _, ok := c.Allowed("golangci", nil, "x/.golangci.yaml"); !ok {
		t.Error("rule-wide entry")
	}
	if _, ok := c.AllowedByPath("golangci", "x/.golangci.yaml"); ok {
		t.Error("by path: rule-wide entry matched")
	}
	if a, ok := c.AllowedByPath("scaffold", "ui/eslint.config.js"); !ok || a.Why != "own rule" {
		t.Errorf("by path: %+v %v", a, ok)
	}
	if _, ok := c.AllowedByPath("scaffold", ""); ok {
		t.Error("by path: empty path matched")
	}
}

// A backslash is part of a name on every OS: ship.yaml spells paths with /, matching never depends on the OS.
func TestAllowed_whenBackslashPath_thenNoMatch(t *testing.T) {
	c, err := Parse([]byte("allow:\n  - { rule: scaffold, path: ui/x.js, why: a }\n  - { rule: golangci, path: 'ui\\y.yaml', why: b }\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Allowed("scaffold", nil, `ui\x.js`); ok {
		t.Error(`ui\x.js matched ui/x.js`)
	}
	if _, ok := c.AllowedByPath("golangci", "ui/y.yaml"); ok {
		t.Error(`ui/y.yaml matched ui\y.yaml`)
	}
	if _, ok := c.AllowedByPath("golangci", `ui\y.yaml`); !ok {
		t.Error(`ui\y.yaml did not match itself`)
	}
}

// A path entry wins over a path-less one listed first: conform and init quote the same why.
func TestAllowed_whenPathAndPathlessMatch_thenPathWins(t *testing.T) {
	c, err := Parse([]byte("allow:\n  - { rule: scaffold, why: wide }\n  - { rule: scaffold, path: ./Makefile, why: own }\n"))
	if err != nil {
		t.Fatal(err)
	}
	if a, ok := c.Allowed("scaffold", nil, "Makefile"); !ok || a.Why != "own" {
		t.Errorf("path entry: %+v %v", a, ok)
	}
	if a, ok := c.AllowedByPath("scaffold", "Makefile"); !ok || a.Why != "own" {
		t.Errorf("by path: %+v %v", a, ok)
	}
	if a, ok := c.Allowed("scaffold", nil, "ci.yaml"); !ok || a.Why != "wide" {
		t.Errorf("other file: %+v %v", a, ok)
	}
}

func TestParse_whenAllowWithoutWhyOrBadVisibility_thenError(t *testing.T) {
	for _, s := range []string{"allow:\n  - { rule: scaffold }\n", "allow:\n  - { why: x }\n", "visibility: internal\n"} {
		if _, err := Parse([]byte(s)); err == nil {
			t.Errorf("%q accepted", s)
		}
	}
}
