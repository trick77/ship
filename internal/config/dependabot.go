package config

import (
	"fmt"
	"slices"
	"strings"
	"unicode"
)

// Dependabot is what ship's dependabot.yaml cannot know of the repo: versions it must never be offered.
type Dependabot struct {
	Ignore []Ignore `yaml:"ignore"`
}

// Ignore keeps one dependency out of an ecosystem's updates, every directory of it. Why is required and lands in
// dependabot.yaml beside the entry: an ignore nobody can explain is a pin nobody revisits.
type Ignore struct {
	Ecosystem  string `yaml:"ecosystem"`
	Dependency string `yaml:"dependency"`
	// Versions are Dependabot's version requirements (">=0.32.0"); empty = every version.
	Versions []string `yaml:"versions"`
	Why      string   `yaml:"why"`
}

// Ecosystems are the package ecosystems ship's dependabot.yaml writes.
var Ecosystems = []string{"gomod", "npm", "docker", "docker-compose", "github-actions"}

func (c *Config) validateDependabot() error {
	for i, ig := range c.Dependabot.Ignore {
		if !slices.Contains(Ecosystems, ig.Ecosystem) {
			return fmt.Errorf("%s: dependabot.ignore[%d]: ecosystem %q, want one of %s", File, i, ig.Ecosystem,
				strings.Join(Ecosystems, ", "))
		}
		if strings.TrimSpace(ig.Dependency) == "" || strings.TrimSpace(ig.Why) == "" {
			return fmt.Errorf("%s: dependabot.ignore[%d] needs dependency and why", File, i)
		}
		// Rendered as one comment line and quoted strings: a line break (YAML counts U+2028 and NEL too) would break
		// out of either, a control character makes the file unparseable.
		for _, s := range append([]string{ig.Dependency, ig.Why}, ig.Versions...) {
			if strings.TrimSpace(s) == "" || strings.IndexFunc(s, func(r rune) bool { return !unicode.IsPrint(r) && r != ' ' }) >= 0 {
				return fmt.Errorf("%s: dependabot.ignore[%d]: printable text on one line, no empty version", File, i)
			}
		}
	}
	return nil
}
