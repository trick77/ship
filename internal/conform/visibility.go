package conform

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/trick77/ship/internal/gitx"
)

// visibilityTTL: a repo's visibility rarely changes; one API call a day per clone keeps the pre-push gate off
// GitHub's unauthenticated rate limit (60 an hour).
const visibilityTTL = 24 * time.Hour

// visibility is the repo's GitHub visibility: the Actions event's repository.private on a runner, else GitHub's
// API without a token (200 public, 404 private: a private repo is invisible to an anonymous caller), cached in the
// git dir.
func (e *env) visibility() (string, error) {
	if e.o.Getenv("GITHUB_ACTIONS") == "true" {
		if p := e.o.Getenv("GITHUB_EVENT_PATH"); p != "" {
			if v, ok := eventVisibility(p); ok {
				return v, nil
			}
		}
	}
	if e.owner == "" {
		return "", errors.New("no GitHub origin remote: cannot ask GitHub for the repo's visibility")
	}
	cache, cerr := gitx.Git(e.root, "rev-parse", "--git-path", "ship/visibility")
	if cerr == nil && !filepath.IsAbs(cache) {
		cache = filepath.Join(e.root, cache)
	}
	if cerr == nil {
		if st, err := os.Stat(cache); err == nil && e.o.Now().Sub(st.ModTime()) < visibilityTTL {
			if b, err := os.ReadFile(cache); err == nil { //nolint:gosec // ship's own cache in the git dir
				if v := strings.TrimSpace(string(b)); v == "public" || v == "private" {
					return v, nil
				}
			}
		}
	}
	v, err := e.askGitHub()
	if err != nil {
		return "", err
	}
	if cerr == nil {
		if err := os.MkdirAll(filepath.Dir(cache), 0o750); err == nil {
			_ = os.WriteFile(cache, []byte(v+"\n"), 0o600) // a lost cache costs one more call, never a result
		}
	}
	return v, nil
}

func (e *env) askGitHub() (string, error) {
	u := fmt.Sprintf("%s/repos/%s/%s", strings.TrimSuffix(e.o.API, "/"), e.owner, e.repo)
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := e.o.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("cannot ask GitHub for the repo's visibility and no cached answer: %w (retry online; "+
			"the answer is cached for a day)", err)
	}
	_ = resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return "public", nil
	case http.StatusNotFound:
		return "private", nil
	}
	return "", fmt.Errorf("GitHub API %s: %s (rate limit? the answer is cached for a day once it succeeds)", u, resp.Status)
}

// eventVisibility reads repository.private from an Actions event payload.
func eventVisibility(p string) (string, bool) {
	b, err := os.ReadFile(p) //nolint:gosec // the runner's event payload
	if err != nil {
		return "", false
	}
	var ev struct {
		Repository *struct {
			Private *bool `json:"private"`
		} `json:"repository"`
	}
	if json.Unmarshal(b, &ev) != nil || ev.Repository == nil || ev.Repository.Private == nil {
		return "", false
	}
	if *ev.Repository.Private {
		return "private", true
	}
	return "public", true
}
