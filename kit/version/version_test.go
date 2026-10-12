package version

import (
	"runtime/debug"
	"testing"
)

func TestResolve(t *testing.T) {
	vcs := func(revision, modified string) *debug.BuildInfo {
		return &debug.BuildInfo{Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: revision},
			{Key: "vcs.modified", Value: modified},
		}}
	}
	tests := []struct {
		name    string
		stamped string
		info    *debug.BuildInfo
		want    string
	}{
		{"stamped wins", "1.2.3", vcs("0123456789abcdef", "true"), "1.2.3"},
		{"clean revision", "dev", vcs("0123456789abcdef", "false"), "dev-0123456"},
		{"dirty revision", "dev", vcs("0123456789abcdef", "true"), "dev-0123456-dirty"},
		{"short revision", "dev", vcs("abc", "false"), "dev-abc"},
		{"no vcs settings", "dev", &debug.BuildInfo{}, "dev"},
		{"no build info", "dev", nil, "dev"},
		{"empty stamp stays empty", "", vcs("0123456789abcdef", "false"), ""},
		{"empty stamp, no build info", "", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolve(tt.stamped, tt.info); got != tt.want {
				t.Errorf("resolve(%q) = %q, want %q", tt.stamped, got, tt.want)
			}
		})
	}
}

func TestStringKeepsAStamp(t *testing.T) {
	old := Version
	t.Cleanup(func() { Version = old })
	Version = "1.2.3"
	if got := String(); got != "1.2.3" {
		t.Errorf("String() = %q, want the stamp 1.2.3", got)
	}
}
