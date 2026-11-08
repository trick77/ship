package pins

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Get fetches url with the given headers: the body of a 200, an error otherwise.
type Get func(url string, header map[string]string) ([]byte, error)

// Change is one pin moved to its newest release.
type Change struct{ Name, From, To string }

// pin is a version Dependabot cannot read (a Go const here, its README example), with where its newest release is.
type pin struct {
	name   string
	files  map[string]*regexp.Regexp // file (repo-relative) → the version, the regexp's second group
	latest func(Get) (string, error)
}

var tracked = []pin{
	{name: "golangci-lint", files: map[string]*regexp.Regexp{
		"internal/pins/pins.go": regexp.MustCompile(`(?m)^(\tGolangci = ")([^"]+)(")`),
		"README.md":             regexp.MustCompile(`(?m)^(lint: )(v[0-9.]+)()`),
	}, latest: proxyLatest("github.com/golangci/golangci-lint/v2")}, // /v2: a v3 needs its config moved by hand
	{name: "govulncheck", files: map[string]*regexp.Regexp{
		"internal/pins/pins.go": regexp.MustCompile(`(?m)^(\tGovulncheck = ")([^"]+)(")`),
	}, latest: proxyLatest("golang.org/x/vuln")},
	{name: "trivy", files: map[string]*regexp.Regexp{
		"internal/pins/pins.go": regexp.MustCompile(`(?m)^(\tTrivy = ")([^"]+)(")`),
	}, latest: trivyLatest},
}

// Update moves every pin under root to its newest release, never down, and returns what moved.
func Update(root string, get Get) ([]Change, error) {
	var out []Change
	for _, p := range tracked {
		to, err := p.latest(get)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p.name, err)
		}
		if !version.MatchString(to) {
			return nil, fmt.Errorf("%s: latest %q is not a version", p.name, to)
		}
		var from string
		for f, re := range p.files {
			path := filepath.Join(root, f)
			b, err := os.ReadFile(path) //nolint:gosec // root + a path from tracked, no input
			if err != nil {
				return nil, err
			}
			m := re.FindSubmatch(b)
			if m == nil {
				return nil, fmt.Errorf("%s: no %s pin in %s", p.name, p.name, f)
			}
			if f == "internal/pins/pins.go" {
				from = string(m[2])
			}
			if !newer(to, string(m[2])) {
				continue
			}
			b = re.ReplaceAll(b, []byte("${1}"+to+"${3}"))
			if err := os.WriteFile(path, b, 0o644); err != nil { //nolint:gosec // a source file, as git checks it out
				return nil, err
			}
		}
		if newer(to, from) {
			out = append(out, Change{p.name, from, to})
		}
	}
	return out, nil
}

// Subject is the commit subject for changes: "pins: golangci-lint v2.15.1, trivy 0.77.0".
func Subject(cs []Change) string {
	s := make([]string, len(cs))
	for i, c := range cs {
		s[i] = c.Name + " " + c.To
	}
	return "pins: " + strings.Join(s, ", ")
}

var version = regexp.MustCompile(`^v?[0-9]+(\.[0-9]+){2}$`)

// newer: a is a higher version than b (v-prefix ignored); a b that is no version is always older.
func newer(a, b string) bool {
	if !version.MatchString(b) {
		return true
	}
	pa, pb := strings.Split(strings.TrimPrefix(a, "v"), "."), strings.Split(strings.TrimPrefix(b, "v"), ".")
	for i := range pa {
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		if x != y {
			return x > y
		}
	}
	return false
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

// trivyLatest: the newest GitHub release whose image is on ghcr already (the release can precede its push).
func trivyLatest(get Get) (string, error) {
	b, err := get("https://api.github.com/repos/aquasecurity/trivy/releases/latest",
		map[string]string{"Accept": "application/vnd.github+json"})
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

// HTTPGet is Get over the network.
func HTTPGet(url string, header map[string]string) ([]byte, error) {
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
