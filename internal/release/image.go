package release

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/trick77/ship/internal/gitx"
	"github.com/trick77/ship/internal/pins"
)

// ociLabels are the image's org.opencontainers.image.* labels. created is HEAD's commit time, not the clock: a
// rebuild of the same commit gets the same labels.
func ociLabels(root, head, repo string, v Version) ([]string, error) {
	ct, err := gitx.Git(root, "log", "-1", "--format=%ct", head)
	if err != nil {
		return nil, err
	}
	sec, err := strconv.ParseInt(ct, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("commit time %q: %w", ct, err)
	}
	const p = "org.opencontainers.image."
	return []string{
		p + "version=" + v.String(),
		p + "revision=" + head,
		p + "source=https://github.com/" + repo,
		p + "created=" + time.Unix(sec, 0).UTC().Format(time.RFC3339),
	}, nil
}

// digestRef is name@digest of the index buildx pushed, read from its --metadata-file. A dry run pushed nothing:
// a placeholder stands in.
func digestRef(name, metaFile string, dry bool) (string, error) {
	if dry {
		return name + "@<digest>", nil
	}
	b, err := os.ReadFile(metaFile) //nolint:gosec // ship's own temp file
	if err != nil {
		return "", fmt.Errorf("image %s: buildx metadata: %w", name, err)
	}
	var m struct {
		Digest string `json:"containerimage.digest"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return "", fmt.Errorf("image %s: buildx metadata: %w", name, err)
	}
	if !strings.HasPrefix(m.Digest, "sha256:") {
		return "", fmt.Errorf("image %s: no digest in buildx metadata %s", name, b)
	}
	return name + "@" + m.Digest, nil
}

// trivyArgs is the docker run of the pinned trivy image scanning ref's platform (an index holds several, trivy
// scans one): HIGH and CRITICAL with a fix available fail.
// The image comes from ghcr, which the runner is logged in to already (no anonymous Docker Hub pull limit). The
// runner's docker login (ghcr, a private package) is mounted read-only: the container has none of its own.
// A named volume keeps the vulnerability DB across the images of one release.
func trivyArgs(ref, platform string) []string {
	args := []string{"run", "--rm", "-v", "ship-trivy-cache:/root/.cache/trivy"}
	if home, err := os.UserHomeDir(); err == nil {
		if cfg := filepath.Join(home, ".docker", "config.json"); fileExists(cfg) {
			args = append(args, "-v", cfg+":/root/.docker/config.json:ro")
		}
	}
	return append(args, "ghcr.io/aquasecurity/trivy:"+pins.Trivy, "image", "--scanners", "vuln", "--no-progress", "--exit-code", "1",
		"--severity", "HIGH,CRITICAL", "--ignore-unfixed", "--platform", platform, ref)
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}
