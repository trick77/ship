package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/trick77/ship/internal/release"
)

// Get fetches url with the given headers: the body of a 200, an error otherwise.
type Get func(url string, header map[string]string) ([]byte, error)

// Change is one pin moved to its newest release.
type Change struct{ Name, From, To string }

// pin is a version Dependabot cannot read (a Go const, its README example), with where its newest release is.
type pin struct {
	name   string
	prefix string                    // the version's "v", or "" (trivy's image tags)
	files  map[string]*regexp.Regexp // file (repo-relative) → the version, the regexp's second group
	latest func(Get) (string, error) // errNotYet: no newer release usable yet
}

const pinsGo = "internal/pins/pins.go"

// errNotYet: no release usable yet (all within the cooldown, or no trivy image): no change, no failure.
var errNotYet = errors.New("not usable yet")

// The cooldown counts from a GitHub release's published_at: the proxy's .info Time is the tagged commit's, which a
// tag on an old (or backdated) commit puts past any cooldown on day one. govulncheck has no GitHub releases (Go
// team, go.googlesource): @latest, no cooldown, retractions honored.
var tracked = []pin{
	{name: "golangci-lint", prefix: "v", files: map[string]*regexp.Regexp{
		pinsGo:      regexp.MustCompile(`(?m)^(\tGolangci = ")([^"]+)(")`),
		"README.md": regexp.MustCompile(`(?m)^(lint: )(v[0-9.]+)()`),
	}, latest: golangciLatest},
	{name: "govulncheck", prefix: "v", files: map[string]*regexp.Regexp{
		pinsGo: regexp.MustCompile(`(?m)^(\tGovulncheck = ")([^"]+)(")`),
	}, latest: proxyLatest("golang.org/x/vuln")},
	{name: "trivy", files: map[string]*regexp.Regexp{
		pinsGo: regexp.MustCompile(`(?m)^(\tTrivy = ")([^"]+)(")`),
	}, latest: trivyLatest},
}

// Update moves every pin under root to its newest release, never down, and returns what moved. A pin whose lookup
// fails is skipped, its error joined into the one returned: the others still move.
func Update(root string, get Get) ([]Change, error) {
	var out []Change
	var errs []error
	for _, p := range tracked {
		c, err := p.update(root, get)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p.name, err))
		} else if c != nil {
			out = append(out, *c)
		}
	}
	return out, errors.Join(errs...)
}

// update moves p's files that are behind its newest release; nil when none was. Every file is read and matched
// before any is written: a pin that fails leaves no half edit for another pin's commit to carry.
func (p pin) update(root string, get Get) (*Change, error) {
	to, err := p.latest(get)
	if errors.Is(err, errNotYet) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !valid(to, p.prefix) {
		return nil, fmt.Errorf("latest %q is not a %sX.Y.Z version", to, p.prefix)
	}
	writes := map[string][]byte{}
	var from string // the oldest version moved
	for f, re := range p.files {
		b, err := os.ReadFile(filepath.Join(root, f)) //nolint:gosec // root + a path from tracked, no input
		if err != nil {
			return nil, err
		}
		m := re.FindSubmatch(b)
		if m == nil {
			return nil, fmt.Errorf("no pin in %s", f)
		}
		cur := string(m[2])
		if !newer(to, cur, p.prefix) {
			continue
		}
		if from == "" || newer(from, cur, p.prefix) {
			from = cur
		}
		writes[f] = re.ReplaceAll(b, []byte("${1}"+to+"${3}"))
	}
	for f, b := range writes {
		if err := os.WriteFile(filepath.Join(root, f), b, 0o644); err != nil { //nolint:gosec // a source file, as git checks it out
			return nil, err
		}
	}
	if len(writes) == 0 {
		return nil, nil
	}
	return &Change{p.name, from, to}, nil
}

// Subject is the commit subject for changes: "pins: golangci-lint v2.15.1, trivy 0.77.0".
func Subject(cs []Change) string {
	s := make([]string, len(cs))
	for i, c := range cs {
		s[i] = c.Name + " " + c.To
	}
	return "pins: " + strings.Join(s, ", ")
}

// valid: s is exactly prefix + X.Y.Z.
func valid(s, prefix string) bool { return prefix+release.Latest([]string{s}, prefix).String() == s }

// newer: to is a higher version than cur; a cur that is no version is always older.
func newer(to, cur, prefix string) bool {
	return to != cur && prefix+release.Latest([]string{cur, to}, prefix).String() == to
}

// cooldown: a release is taken once published this long, time for a hijacked or broken one to be pulled. It is
// Dependabot's (dependabot.yaml default-days), a test keeps them one.
const cooldown = 72 * time.Hour

// walk caps the releases newest tries, from the highest down.
const walk = 10

// newest is the highest of versions (prefix + X.Y.Z; others ignored) ok accepts, walking down from the highest:
// a project releasing faster than the cooldown still moves, to its newest release old enough. errNotYet when none.
func newest(versions []string, prefix string, ok func(v string) (bool, error)) (string, error) {
	var cands []string
	for _, v := range versions {
		if valid(v, prefix) {
			cands = append(cands, v)
		}
	}
	for i := 0; i < walk && len(cands) > 0; i++ {
		v := prefix + release.Latest(cands, prefix).String()
		good, err := ok(v)
		if err != nil {
			return "", err
		}
		if good {
			return v, nil
		}
		cands = slices.DeleteFunc(cands, func(c string) bool { return c == v })
	}
	return "", errNotYet
}

// aged: t is past the cooldown.
func aged(t time.Time) bool { return !t.IsZero() && time.Since(t) >= cooldown }

// githubLatest: the newest of repo's releases (no drafts, no pre-releases) published past the cooldown that ok
// accepts. version maps a release tag to the pin's version, "" drops the release. With GITHUB_TOKEN (the workflow's)
// the API call is authenticated: unauthenticated, a shared runner IP may be out of its 60 an hour.
func githubLatest(get Get, repo, prefix string, version func(tag string) string,
	ok func(v string) (bool, error)) (string, error) {
	h := map[string]string{"Accept": "application/vnd.github+json"}
	if t := os.Getenv("GITHUB_TOKEN"); t != "" {
		h["Authorization"] = "Bearer " + t
	}
	// 10, walk's: no more is tried. Each release carries its assets (~100 KB): a page of 30 runs to MBs.
	b, err := get("https://api.github.com/repos/"+repo+"/releases?per_page=10", h)
	if err != nil {
		return "", err
	}
	var rels []struct {
		Tag        string    `json:"tag_name"`
		Published  time.Time `json:"published_at"`
		Draft      bool      `json:"draft"`
		Prerelease bool      `json:"prerelease"`
	}
	if err := json.Unmarshal(b, &rels); err != nil {
		return "", err
	}
	published := map[string]time.Time{}
	for _, r := range rels {
		if v := version(r.Tag); v != "" && !r.Draft && !r.Prerelease {
			published[v] = r.Published
		}
	}
	return newest(slices.Collect(maps.Keys(published)), prefix, func(v string) (bool, error) {
		if !aged(published[v]) {
			return false, nil
		}
		return ok(v)
	})
}

const golangciModule = "github.com/golangci/golangci-lint/v2"

// golangciLatest: the newest v2 release past the cooldown, never a retracted one (v3: its config moves by hand).
func golangciLatest(get Get) (string, error) {
	retracted, err := retractions(get, golangciModule)
	if err != nil {
		return "", err
	}
	return githubLatest(get, "golangci/golangci-lint", "v", func(tag string) string {
		if strings.HasPrefix(tag, "v2.") {
			return tag
		}
		return ""
	}, func(v string) (bool, error) { return !retracted(v), nil })
}

// proxyLatest: the module proxy's @latest, the newest release go get takes (no pre-release, no retracted one).
func proxyLatest(module string) func(Get) (string, error) {
	return func(get Get) (string, error) {
		b, err := get("https://proxy.golang.org/"+module+"/@latest", nil)
		if err != nil {
			return "", err
		}
		var v struct{ Version string }
		if err := json.Unmarshal(b, &v); err != nil {
			return "", err
		}
		return v.Version, nil
	}
}

// retractions reads the retract directives of module's latest go.mod, the ones the go command honors: a broken
// release is retracted by the next, which the cooldown may still hold back while the broken one aged past it.
func retractions(get Get, module string) (func(v string) bool, error) {
	latest, err := proxyLatest(module)(get)
	if err != nil {
		return nil, err
	}
	b, err := get("https://proxy.golang.org/"+module+"/@v/"+latest+".mod", nil)
	if err != nil {
		return nil, err
	}
	var spans [][2]string // [lo, hi], a single version as lo = hi
	var block bool
	for _, line := range strings.Split(string(b), "\n") {
		line, _, _ = strings.Cut(line, "//")
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "retract") && strings.HasSuffix(line, "("):
			block = true
			continue
		case block && line == ")":
			block = false
			continue
		case strings.HasPrefix(line, "retract "):
			line = strings.TrimSpace(strings.TrimPrefix(line, "retract "))
		case !block:
			continue
		}
		if lo, hi, ok := strings.Cut(strings.Trim(line, "[]"), ","); ok && strings.HasPrefix(line, "[") {
			spans = append(spans, [2]string{strings.TrimSpace(lo), strings.TrimSpace(hi)})
		} else if line != "" {
			spans = append(spans, [2]string{line, line})
		}
	}
	return func(v string) bool {
		for _, s := range spans {
			if !newer(s[0], v, "v") && !newer(v, s[1], "v") {
				return true
			}
		}
		return false
	}, nil
}

// trivyLatest: the newest release past the cooldown whose image is on ghcr (the release can precede its push).
func trivyLatest(get Get) (string, error) {
	var token string
	return githubLatest(get, "aquasecurity/trivy", "", func(tag string) string { return strings.TrimPrefix(tag, "v") },
		func(tag string) (bool, error) {
			if token == "" {
				b, err := get("https://ghcr.io/token?scope=repository:aquasecurity/trivy:pull", nil)
				if err != nil {
					return false, err
				}
				var tok struct{ Token string }
				if err := json.Unmarshal(b, &tok); err != nil {
					return false, err
				}
				if tok.Token == "" {
					return false, errors.New("ghcr: empty pull token")
				}
				token = tok.Token
			}
			_, err := get("https://ghcr.io/v2/aquasecurity/trivy/manifests/"+tag, map[string]string{
				"Authorization": "Bearer " + token,
				"Accept":        "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json",
			})
			var se *statusError
			if errors.As(err, &se) && se.code == http.StatusNotFound {
				return false, nil // not pushed (yet): the next older release
			}
			return err == nil, err // ghcr failing is no reason to pin an older trivy
		})
}

// statusError is a response other than 200.
type statusError struct {
	url, status string
	code        int
}

func (e *statusError) Error() string { return "GET " + e.url + ": " + e.status }

// httpGet is Get over the network.
func httpGet(url string, header map[string]string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	res, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return nil, &statusError{url, res.Status, res.StatusCode}
	}
	const limit = 8 << 20
	b, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if len(b) > limit {
		return nil, fmt.Errorf("GET %s: body over %d bytes", url, limit)
	}
	return b, nil
}
