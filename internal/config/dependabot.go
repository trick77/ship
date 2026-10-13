package config

import (
	"fmt"
	"slices"
	"strings"
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

// IgnoreFor returns the ignore entries of one ecosystem, in ship.yaml's order.
func (c *Config) IgnoreFor(ecosystem string) []Ignore {
	var ig []Ignore
	for _, i := range c.Dependabot.Ignore {
		if i.Ecosystem == ecosystem {
			ig = append(ig, i)
		}
	}
	return ig
}

func (c *Config) validateDependabot() error {
	for i, ig := range c.Dependabot.Ignore {
		if !slices.Contains(Ecosystems, ig.Ecosystem) {
			return fmt.Errorf("%s: dependabot.ignore[%d]: ecosystem %q, want one of %s", File, i, ig.Ecosystem,
				strings.Join(Ecosystems, ", "))
		}
		if ig.Dependency == "" || ig.Why == "" {
			return fmt.Errorf("%s: dependabot.ignore[%d] needs dependency and why", File, i)
		}
		// Rendered as one comment line and quoted strings: a newline would break out of either.
		if strings.ContainsAny(ig.Why+ig.Dependency+strings.Join(ig.Versions, ""), "\n\r") ||
			slices.Contains(ig.Versions, "") {
			return fmt.Errorf("%s: dependabot.ignore[%d]: one line each, no empty version", File, i)
		}
	}
	return nil
}
