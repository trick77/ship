package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
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
	src    string                    // the file whose version is the pin's, one of files
	files  map[string]*regexp.Regexp // file (repo-relative) → the version, the regexp's second group
	latest func(Get) (string, error)
}

const pinsGo = "internal/pins/pins.go"

var tracked = []pin{
	{name: "golangci-lint", prefix: "v", src: pinsGo, files: map[string]*regexp.Regexp{
		pinsGo:      regexp.MustCompile(`(?m)^(\tGolangci = ")([^"]+)(")`),
		"README.md": regexp.MustCompile(`(?m)^(lint: )(v[0-9.]+)()`),
	}, latest: proxyLatest("github.com/golangci/golangci-lint/v2")}, // /v2: a v3 needs its config moved by hand
	{name: "govulncheck", prefix: "v", src: pinsGo, files: map[string]*regexp.Regexp{
		pinsGo: regexp.MustCompile(`(?m)^(\tGovulncheck = ")([^"]+)(")`),
	}, latest: proxyLatest("golang.org/x/vuln")},
	{name: "trivy", src: pinsGo, files: map[string]*regexp.Regexp{
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

// update moves p's files that are behind its newest release; nil when none was.
func (p pin) update(root string, get Get) (*Change, error) {
	to, err := p.latest(get)
	if err != nil {
		return nil, err
	}
	if !valid(to, p.prefix) {
		return nil, fmt.Errorf("latest %q is not a %sX.Y.Z version", to, p.prefix)
	}
	read := func(f string) ([]byte, string, error) {
		b, err := os.ReadFile(filepath.Join(root, f)) //nolint:gosec // root + a path from tracked, no input
		if err != nil {
			return nil, "", err
		}
		m := p.files[f].FindSubmatch(b)
		if m == nil {
			return nil, "", fmt.Errorf("no pin in %s", f)
		}
		return b, string(m[2]), nil
	}
	_, from, err := read(p.src)
	if err != nil {
		return nil, err
	}
	var moved bool
	for f, re := range p.files {
		b, cur, err := read(f)
		if err != nil {
			return nil, err
		}
		if !newer(to, cur, p.prefix) {
			continue
		}
		b = re.ReplaceAll(b, []byte("${1}"+to+"${3}"))
		if err := os.WriteFile(filepath.Join(root, f), b, 0o644); err != nil { //nolint:gosec // a source file, as git checks it out
			return nil, err
		}
		moved = true
	}
	if !moved {
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

// proxyLatest: the module proxy's @latest, the newest release go get would take (no pre-release).
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

// trivyLatest: the newest GitHub release whose image is on ghcr already (the release can precede its push). With
// GITHUB_TOKEN (the workflow's) the API call is authenticated: unauthenticated, a shared runner IP may be out of its
// 60 an hour.
func trivyLatest(get Get) (string, error) {
	h := map[string]string{"Accept": "application/vnd.github+json"}
	if t := os.Getenv("GITHUB_TOKEN"); t != "" {
		h["Authorization"] = "Bearer " + t
	}
	b, err := get("https://api.github.com/repos/aquasecurity/trivy/releases/latest", h)
	if err != nil {
		return "", err
	}
	var rel struct {
		Tag string `json:"tag_name"`
	}
	if err := json.Unmarshal(b, &rel); err != nil {
		return "", err
	}
	tag := strings.TrimPrefix(rel.Tag, "v")
	if b, err = get("https://ghcr.io/token?scope=repository:aquasecurity/trivy:pull", nil); err != nil {
		return "", err
	}
	var tok struct{ Token string }
	if err := json.Unmarshal(b, &tok); err != nil {
		return "", err
	}
	if _, err := get("https://ghcr.io/v2/aquasecurity/trivy/manifests/"+tag, map[string]string{
		"Authorization": "Bearer " + tok.Token,
		"Accept":        "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json",
	}); err != nil {
		return "", fmt.Errorf("image %s not on ghcr yet: %w", tag, err)
	}
	return tag, nil
}

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
	b, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, res.Status)
	}
	return b, nil
}
