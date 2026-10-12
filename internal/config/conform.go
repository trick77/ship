package config

import "fmt"

// Allow excuses one conform rule, optionally for one image or one file only. Why is required: an exception nobody
// can explain is a drift nobody decided.
type Allow struct {
	Rule string `yaml:"rule"`
	// Image is an image suffix ("" the repo's own image); unset = every image.
	Image *string `yaml:"image"`
	// Path is repo-relative; empty = every file.
	Path string `yaml:"path"`
	Why  string `yaml:"why"`
}

// Allowed returns the entry excusing rule for image (nil: no image) and path, if any.
func (c *Config) Allowed(rule string, image *string, path string) (Allow, bool) {
	for _, a := range c.Allow {
		if a.Rule != rule || a.Path != "" && a.Path != path {
			continue
		}
		if a.Image != nil && (image == nil || *a.Image != *image) {
			continue
		}
		return a, true
	}
	return Allow{}, false
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
