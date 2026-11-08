package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/trick77/ship/internal/pins"
)

// fakeGet answers the upstream lookups with the given versions; a missing trivy image is a 404.
func fakeGet(golangci, vuln, trivy string, image bool) Get {
	return func(url string, h map[string]string) ([]byte, error) {
		switch {
		case strings.Contains(url, "golangci-lint/v2/@latest"):
			return []byte(`{"Version":"` + golangci + `"}`), nil
		case strings.Contains(url, "x/vuln/@latest"):
			return []byte(`{"Version":"` + vuln + `"}`), nil
		case strings.Contains(url, "trivy/releases/latest"):
			return []byte(`{"tag_name":"v` + trivy + `"}`), nil
		case strings.Contains(url, "ghcr.io/token"):
			return []byte(`{"token":"t"}`), nil
		case strings.HasSuffix(url, "/manifests/"+trivy) && image && h["Authorization"] == "Bearer t":
			return []byte(`{}`), nil
		}
		return nil, errors.New("404 " + url)
	}
}

// down is get with every URL containing part failing.
func down(get Get, part string) Get {
	return func(u string, h map[string]string) ([]byte, error) {
		if strings.Contains(u, part) {
			return nil, errors.New("503 " + u)
		}
		return get(u, h)
	}
}

// repo is a root with pins.go as it is and a README naming lint.
func repo(t *testing.T, lint string) string {
	t.Helper()
	root := t.TempDir()
	src, err := os.ReadFile("../pins.go")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "internal/pins"), 0o750); err != nil {
		t.Fatal(err)
	}
	for f, b := range map[string][]byte{
		pinsGo:      src,
		"README.md": []byte("```yaml\nlint: " + lint + "         # golangci-lint\n```\n"),
	} {
		if err := os.WriteFile(filepath.Join(root, f), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func read(t *testing.T, root, f string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, f))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestUpdate_whenNewer_thenRewritten(t *testing.T) {
	root := repo(t, pins.Golangci)
	cs, err := Update(root, fakeGet("v9.0.0", pins.Govulncheck, "9.1.0", true))
	if err != nil {
		t.Fatal(err)
	}
	if got := Subject(cs); got != "pins: golangci-lint v9.0.0, trivy 9.1.0" {
		t.Errorf("subject %q", got)
	}
	if cs[0].From != pins.Golangci || cs[1].From != pins.Trivy {
		t.Errorf("from: %+v", cs)
	}
	src := read(t, root, pinsGo)
	for _, want := range []string{"\tGolangci = \"v9.0.0\"\n", "\tTrivy = \"9.1.0\"\n",
		"\tGovulncheck = \"" + pins.Govulncheck + "\"\n"} {
		if !strings.Contains(src, want) {
			t.Errorf("pins.go lacks %q", want)
		}
	}
	if got := read(t, root, "README.md"); !strings.Contains(got, "\nlint: v9.0.0         # golangci-lint\n") {
		t.Errorf("README:\n%s", got)
	}
}

func TestUpdate_whenCurrentOrOlder_thenUntouched(t *testing.T) {
	root := repo(t, pins.Golangci)
	before := read(t, root, pinsGo)
	cs, err := Update(root, fakeGet(pins.Golangci, "v0.0.1", pins.Trivy, true))
	if err != nil || len(cs) != 0 {
		t.Fatalf("%v %+v", err, cs)
	}
	if read(t, root, pinsGo) != before {
		t.Error("pins.go rewritten")
	}
}

// README behind a current pins.go is a change too: dropped, it would stay behind for good.
func TestUpdate_whenOnlyReadmeBehind_thenChange(t *testing.T) {
	root := repo(t, "v0.1.0")
	cs, err := Update(root, fakeGet(pins.Golangci, pins.Govulncheck, pins.Trivy, true))
	if err != nil || len(cs) != 1 || cs[0].Name != "golangci-lint" {
		t.Fatalf("%v %+v", err, cs)
	}
	if got := read(t, root, "README.md"); !strings.Contains(got, "lint: "+pins.Golangci+" ") {
		t.Errorf("README:\n%s", got)
	}
}

// README behind pins.go: From is the oldest version moved, not pins.go's current one.
func TestUpdate_whenFilesDiffer_thenFromOldest(t *testing.T) {
	cs, err := Update(repo(t, "v0.1.0"), fakeGet("v9.0.0", pins.Govulncheck, pins.Trivy, true))
	if err != nil || len(cs) != 1 || cs[0].From != "v0.1.0" || cs[0].To != "v9.0.0" {
		t.Fatalf("%v %+v", err, cs)
	}
}

// One upstream failing never holds the others back.
func TestUpdate_whenOneUpstreamBroken_thenOthersMove(t *testing.T) {
	root := repo(t, pins.Golangci)
	cs, err := Update(root, down(fakeGet("v9.0.0", pins.Govulncheck, "9.1.0", true), "trivy/releases"))
	if err == nil || !strings.Contains(err.Error(), "trivy: 503") {
		t.Errorf("err %v", err)
	}
	if len(cs) != 1 || cs[0].Name != "golangci-lint" || !strings.Contains(read(t, root, pinsGo), `"v9.0.0"`) {
		t.Errorf("changes %+v", cs)
	}
}

// A trivy release whose image is not on ghcr yet is no change and no failure: the next run takes it.
func TestUpdate_whenTrivyImageMissing_thenNotYet(t *testing.T) {
	root := repo(t, pins.Golangci)
	cs, err := Update(root, fakeGet(pins.Golangci, pins.Govulncheck, "9.1.0", false))
	if err != nil || len(cs) != 0 || !strings.Contains(read(t, root, pinsGo), `"`+pins.Trivy+`"`) {
		t.Errorf("%v %+v", err, cs)
	}
}

// A pin failing in its second file writes neither: no half edit for another pin's commit.
func TestUpdate_whenSecondFileBroken_thenNothingWritten(t *testing.T) {
	root := repo(t, pins.Golangci)
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("no lint key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := read(t, root, pinsGo)
	if _, err := Update(root, fakeGet("v9.0.0", pins.Govulncheck, pins.Trivy, true)); err == nil {
		t.Fatal("no error")
	}
	if read(t, root, pinsGo) != before {
		t.Error("pins.go half edited")
	}
}

func TestUpdate_whenUpstreamBroken_thenError(t *testing.T) {
	for name, get := range map[string]Get{
		"not a version": fakeGet("latest", pins.Govulncheck, pins.Trivy, true),
		"no prefix":     fakeGet("2.99.0", pins.Govulncheck, pins.Trivy, true),
		"bad json":      func(string, map[string]string) ([]byte, error) { return []byte("<html>"), nil },
		"down":          func(u string, _ map[string]string) ([]byte, error) { return nil, errors.New("503 " + u) },
	} {
		if _, err := Update(repo(t, pins.Golangci), get); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestUpdate_whenPinMissing_thenError(t *testing.T) {
	root := repo(t, pins.Golangci)
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("no lint key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Update(root, fakeGet("v9.0.0", pins.Govulncheck, pins.Trivy, true)); err == nil ||
		!strings.Contains(err.Error(), "README.md") {
		t.Errorf("err %v", err)
	}
}

func TestTrivyLatest_whenToken_thenAuthenticated(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "gh")
	var auth string
	get := fakeGet(pins.Golangci, pins.Govulncheck, "9.1.0", true)
	_, err := trivyLatest(func(u string, h map[string]string) ([]byte, error) {
		if strings.HasPrefix(u, "https://api.github.com/") {
			auth = h["Authorization"]
		}
		return get(u, h)
	})
	if err != nil || auth != "Bearer gh" {
		t.Errorf("%v, Authorization %q", err, auth)
	}
}

// The tracked regexps find every pin in the real files: a reworded const or README line would stop its updates.
func TestTracked_whenRealFiles_thenEveryPinFound(t *testing.T) {
	for _, p := range tracked {
		for f, re := range p.files {
			b, err := os.ReadFile(filepath.Join("../../..", f))
			if err != nil {
				t.Fatal(err)
			}
			if !re.Match(b) {
				t.Errorf("%s: no pin in %s", p.name, f)
			}
		}
	}
}

func TestRun_whenChangesOrErrors_thenSubjectAndExit(t *testing.T) {
	for _, tc := range []struct {
		name       string
		get        Get
		code       int
		out, inErr string
	}{
		{"current", fakeGet(pins.Golangci, pins.Govulncheck, pins.Trivy, true), 0, "", ""},
		{"moved, one broken", down(fakeGet("v9.0.0", pins.Govulncheck, pins.Trivy, true), "trivy/releases"), 0,
			"pins: golangci-lint v9.0.0\n", "::warning::pins: trivy"},
		{"none moved, broken", down(fakeGet(pins.Golangci, pins.Govulncheck, pins.Trivy, true), "trivy/releases"), 1,
			"", "::error::pins: trivy"},
	} {
		var out, errOut strings.Builder
		code := run(repo(t, pins.Golangci), tc.get, &out, &errOut)
		if code != tc.code || out.String() != tc.out || !strings.Contains(errOut.String(), tc.inErr) {
			t.Errorf("%s: %d %q %q", tc.name, code, out.String(), errOut.String())
		}
	}
}

func TestHTTPGet_whenServed_thenBodyOrStatusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(r.Header.Get("X-Pin")))
	}))
	if b, err := httpGet(srv.URL+"/ok", map[string]string{"X-Pin": "v1"}); err != nil || string(b) != "v1" {
		t.Errorf("ok: %q %v", b, err)
	}
	if _, err := httpGet(srv.URL+"/missing", nil); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("missing: %v", err)
	}
	if _, err := httpGet("http://\x7f", nil); err == nil {
		t.Error("bad url: no error")
	}
	srv.Close()
	if _, err := httpGet(srv.URL, nil); err == nil {
		t.Error("closed server: no error")
	}
}

func TestNewer(t *testing.T) {
	for _, tc := range []struct {
		to, cur, prefix string
		want            bool
	}{
		{"v2.15.0", "v2.14.0", "v", true}, {"v2.14.0", "v2.15.0", "v", false}, {"v2.14.0", "v2.14.0", "v", false},
		{"0.76.2", "0.75.10", "", true}, {"0.10.0", "0.9.9", "", true}, {"v1.0.0", "", "v", true},
	} {
		if got := newer(tc.to, tc.cur, tc.prefix); got != tc.want {
			t.Errorf("newer(%q, %q) = %v", tc.to, tc.cur, got)
		}
	}
}
