// Package version holds the release version, stamped at build time:
// -ldflags "-X github.com/trick77/ship/kit/version.Version=1.2.3".
package version

import "runtime/debug"

// Version is the release tag the binary was built for, "dev" for local builds.
var Version = "dev"

// String is Version, or for an unstamped build dev-<short commit>, -dirty with uncommitted changes, so it stays
// traceable. Go stamps the commit only in go build; go run and container builds (no .git in the context) report
// "dev". An empty stamp (--build-arg VERSION=) stays empty: a broken release stamp must stay visible in the startup
// log, not pass as "dev".
func String() string {
	info, _ := debug.ReadBuildInfo()
	return resolve(Version, info)
}

// resolve is String for a given stamp and build info; info is nil when the binary has none.
func resolve(stamped string, info *debug.BuildInfo) string {
	if stamped != "dev" || info == nil {
		return stamped
	}
	var revision, modified string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value
		}
	}
	if revision == "" {
		return stamped
	}
	if len(revision) > 7 {
		revision = revision[:7]
	}
	v := "dev-" + revision
	if modified == "true" {
		v += "-dirty"
	}
	return v
}
