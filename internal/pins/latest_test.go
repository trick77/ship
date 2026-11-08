package pins

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

func repo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	src, err := os.ReadFile("pins.go")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "internal/pins"), 0o750); err != nil {
		t.Fatal(err)
	}
	for f, b := range map[string][]byte{
		"internal/pins/pins.go": src,
		"README.md":             []byte("```yaml\nlint: " + Golangci + "         # golangci-lint\n```\n"),
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
	root := repo(t)
	cs, err := Update(root, fakeGet("v9.0.0", Govulncheck, "9.1.0", true))
	if err != nil {
		t.Fatal(err)
	}
	if got := Subject(cs); got != "pins: golangci-lint v9.0.0, trivy 9.1.0" {
		t.Errorf("subject %q", got)
	}
	if cs[0].From != Golangci || cs[1].From != Trivy {
		t.Errorf("from: %+v", cs)
	}
	src := read(t, root, "internal/pins/pins.go")
	for _, want := range []string{"\tGolangci = \"v9.0.0\"\n", "\tTrivy = \"9.1.0\"\n", "\tGovulncheck = \"" + Govulncheck + "\"\n"} {
		if !strings.Contains(src, want) {
			t.Errorf("pins.go lacks %q", want)
		}
	}
	if got := read(t, root, "README.md"); !strings.Contains(got, "\nlint: v9.0.0         # golangci-lint\n") {
		t.Errorf("README:\n%s", got)
	}
}

func TestUpdate_whenCurrentOrOlder_thenUntouched(t *testing.T) {
	root := repo(t)
	before := read(t, root, "internal/pins/pins.go")
	cs, err := Update(root, fakeGet(Golangci, "v0.0.1", Trivy, true))
	if err != nil || len(cs) != 0 {
		t.Fatalf("%v %+v", err, cs)
	}
	if read(t, root, "internal/pins/pins.go") != before {
		t.Error("pins.go rewritten")
	}
}

func TestUpdate_whenUpstreamBroken_thenError(t *testing.T) {
	for name, get := range map[string]Get{
		"no trivy image yet": fakeGet(Golangci, Govulncheck, "9.1.0", false),
		"not a version":      fakeGet("latest", Govulncheck, Trivy, true),
		"bad json":           func(string, map[string]string) ([]byte, error) { return []byte("<html>"), nil },
		"down":               func(u string, _ map[string]string) ([]byte, error) { return nil, errors.New("503 " + u) },
	} {
		if _, err := Update(repo(t), get); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func TestUpdate_whenPinMissing_thenError(t *testing.T) {
	root := repo(t)
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("no lint key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Update(root, fakeGet("v9.0.0", Govulncheck, Trivy, true)); err == nil ||
		!strings.Contains(err.Error(), "README.md") {
		t.Errorf("err %v", err)
	}
}

// The tracked regexps find every pin in the real files: a reworded const or README line would stop its updates.
func TestTracked_whenRealFiles_thenEveryPinFound(t *testing.T) {
	for _, p := range tracked {
		for f, re := range p.files {
			b, err := os.ReadFile(filepath.Join("../..", f))
			if err != nil {
				t.Fatal(err)
			}
			if !re.Match(b) {
				t.Errorf("%s: no pin in %s", p.name, f)
			}
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
	if b, err := HTTPGet(srv.URL+"/ok", map[string]string{"X-Pin": "v1"}); err != nil || string(b) != "v1" {
		t.Errorf("ok: %q %v", b, err)
	}
	if _, err := HTTPGet(srv.URL+"/missing", nil); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("missing: %v", err)
	}
	if _, err := HTTPGet("http://\x7f", nil); err == nil {
		t.Error("bad url: no error")
	}
	srv.Close()
	if _, err := HTTPGet(srv.URL, nil); err == nil {
		t.Error("closed server: no error")
	}
}

func TestNewer(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"v2.15.0", "v2.14.0", true}, {"v2.14.0", "v2.15.0", false}, {"v2.14.0", "v2.14.0", false},
		{"0.76.2", "0.75.10", true}, {"0.10.0", "0.9.9", true}, {"v1.0.0", "", true},
	} {
		if got := newer(tc.a, tc.b); got != tc.want {
			t.Errorf("newer(%q, %q) = %v", tc.a, tc.b, got)
		}
	}
}
