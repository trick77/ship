package config

import "fmt"

// Allow excuses one conform rule, optionally for one image or one file only. Why is required: an exception nobody
// can explain is a drift nobody decided.
type Allow struct {
	Rule string `yaml:"rule"`
	// Image is an image suffix ("" the repo's own image); unset = every image.
	Image *string `yaml:"image"`
	// Path is repo-relative; empty = every file. A scaffold allow with a path keeps that file out of ship init for
	// good: canon updates no longer reach it; drop the allow to take them again.
	Path string `yaml:"path"`
	Why  string `yaml:"why"`
}

// Allowed returns the entry excusing rule for image (nil: no image) and path, if any; an entry naming path wins
// over a path-less one, so conform and init quote the same why. Paths compare in CleanPath form: ./ui/x matches ui/x.
func (c *Config) Allowed(rule string, image *string, path string) (Allow, bool) {
	if a, ok := c.byPath(rule, image, path); ok {
		return a, true
	}
	for _, a := range c.Allow {
		if a.Rule == rule && a.Path == "" && imageMatches(a, image) {
			return a, true
		}
	}
	return Allow{}, false
}

// AllowedByPath is Allowed, no image, for the entries naming path only: a path-less entry never matches.
func (c *Config) AllowedByPath(rule, path string) (Allow, bool) { return c.byPath(rule, nil, path) }

func (c *Config) byPath(rule string, image *string, path string) (Allow, bool) {
	if path == "" {
		return Allow{}, false
	}
	for _, a := range c.Allow {
		if a.Rule == rule && a.Path != "" && CleanPath(a.Path) == CleanPath(path) && imageMatches(a, image) {
			return a, true
		}
	}
	return Allow{}, false
}

func imageMatches(a Allow, image *string) bool {
	return a.Image == nil || image != nil && *a.Image == *image
}

func (c *Config) validateConform() error {
	switch c.Visibility {
	case "", "public", "private":
	default:
		return fmt.Errorf("%s: visibility %q: public or private", File, c.Visibility)
	}
	for i, a := range c.Allow {
		if a.Rule == "" || a.Why == "" {
			return fmt.Errorf("%s: allow[%d] needs rule and why", File, i)
		}
	}
	return nil
}
