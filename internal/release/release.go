// Package release cuts a version: next semver tag, images built and pushed, then the tag, then the GitHub release.
//
// The tag is created only after every image push succeeded, so a failed release never leaves an orphan tag; the
// next version is derived from the newest tag, so an orphan would burn its number for good. Tag and release go
// through the GitHub API (gh), so no git push credentials are needed.
package release

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/trick77/ship/internal/config"
	"github.com/trick77/ship/internal/gitx"
)

// Options are the release flags.
type Options struct {
	Bump string // patch, minor, major
	// Cache is passed to buildx as cache backend type, e.g. "gha" in GitHub Actions; empty = no cache flags.
	Cache  string
	DryRun bool
}

// Version is a semver triple.
type Version struct{ Major, Minor, Patch int }

func (v Version) String() string { return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch) }

var semver = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)$`)

// Latest returns the highest semver tag carrying exactly prefix ("" for the family's plain 1.2.3), 0.0.0 when none.
func Latest(tags []string, prefix string) Version {
	var best Version
	for _, t := range tags {
		rest, ok := strings.CutPrefix(strings.TrimSpace(t), prefix)
		if !ok {
			continue
		}
		m := semver.FindStringSubmatch(rest)
		if m == nil {
			continue
		}
		v := Version{atoi(m[1]), atoi(m[2]), atoi(m[3])}
		if less(best, v) {
			best = v
		}
	}
	return best
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

func less(a, b Version) bool {
	if a.Major != b.Major {
		return a.Major < b.Major
	}
	if a.Minor != b.Minor {
		return a.Minor < b.Minor
	}
	return a.Patch < b.Patch
}

// Next bumps v.
func Next(v Version, bump string) (Version, error) {
	switch bump {
	case "patch":
		return Version{v.Major, v.Minor, v.Patch + 1}, nil
	case "minor":
		return Version{v.Major, v.Minor + 1, 0}, nil
	case "major":
		return Version{v.Major + 1, 0, 0}, nil
	}
	return v, fmt.Errorf("bump %q: want patch, minor or major", bump)
}

// Fixed returns the tags that name exactly this build: version and short sha. Pushed with the build.
func Fixed(image string, v Version, short string) []string {
	return []string{fmt.Sprintf("%s:%s", image, v), fmt.Sprintf("%s:%s", image, short)}
}

// Moving returns the tags that follow the newest release: major.minor, major, latest. Set only after the git tag and
// the GitHub release exist, so a failed release never leaves :latest pointing at a version that was never released.
func Moving(image string, v Version) []string {
	return []string{
		fmt.Sprintf("%s:%d.%d", image, v.Major, v.Minor),
		fmt.Sprintf("%s:%d", image, v.Major),
		image + ":latest",
	}
}

// Run releases HEAD. HEAD must be origin/<base> and the tree clean: a release is what master holds, nothing local.
func Run(root string, c *config.Config, o Options) error {
	if _, err := gitx.Git(root, "fetch", "-q", "--tags", "origin", c.Base); err != nil {
		return err
	}
	head, err := gitx.Head(root)
	if err != nil {
		return err
	}
	tip, err := gitx.Git(root, "rev-parse", "origin/"+c.Base)
	if err != nil {
		return err
	}
	if head != tip {
		return fmt.Errorf("HEAD %s is not origin/%s %s: release only what %s holds", head[:12], c.Base, tip[:12], c.Base)
	}
	if dirty, err := gitx.Dirty(root); err != nil {
		return err
	} else if dirty != "" {
		return fmt.Errorf("uncommitted changes:\n%s", dirty)
	}
	tagList, err := gitx.Git(root, "tag", "--list")
	if err != nil {
		return err
	}
	prev := Latest(strings.Split(tagList, "\n"), c.TagPrefix)
	next, err := Next(prev, o.Bump)
	if err != nil {
		return err
	}
	tag := c.TagPrefix + next.String()
	repo, err := repoSlug(root)
	if err != nil {
		return err
	}
	short := head[:7]
	fmt.Printf("ship: release %s -> %s (%s) at %s\n", prev, next, o.Bump, short)

	var images, names []string
	for _, im := range c.Images {
		name := "ghcr.io/" + strings.ToLower(repo) + im.Suffix
		args := []string{"buildx", "build", "--push",
			"--platform", strings.Join(im.Platforms, ","),
			"--build-arg", "VERSION=" + next.String(),
			"-f", im.Containerfile}
		if im.Target != "" {
			args = append(args, "--target", im.Target)
		}
		if o.Cache != "" {
			args = append(args, "--cache-from", "type="+o.Cache, "--cache-to", "type="+o.Cache+",mode=max")
		}
		for _, t := range Fixed(name, next, short) {
			args = append(args, "-t", t)
		}
		args = append(args, im.Context)
		if err := run(root, o.DryRun, "docker", args...); err != nil {
			return fmt.Errorf("image %s: %w", name, err)
		}
		images = append(images, fmt.Sprintf("`%s:%s`", name, next))
		names = append(names, name)
	}

	if err := run(root, o.DryRun, "gh", "api", "repos/"+repo+"/git/refs",
		"-f", "ref=refs/tags/"+tag, "-f", "sha="+head); err != nil {
		return fmt.Errorf("tag: %w", err)
	}
	notes := "Release " + next.String()
	if len(images) > 0 {
		notes = "Container images: " + strings.Join(images, ", ")
	}
	if err := run(root, o.DryRun, "gh", "release", "create", tag, "--repo", repo,
		"--title", tag, "--notes", notes); err != nil {
		return fmt.Errorf("github release: %w", err)
	}
	// Retag the pushed manifest (all platforms) in the registry, no rebuild.
	for _, name := range names {
		args := []string{"buildx", "imagetools", "create"}
		for _, t := range Moving(name, next) {
			args = append(args, "-t", t)
		}
		args = append(args, fmt.Sprintf("%s:%s", name, next))
		if err := run(root, o.DryRun, "docker", args...); err != nil {
			return fmt.Errorf("moving tags of %s (released; rerun: docker %s): %w", name, strings.Join(args, " "), err)
		}
	}
	fmt.Printf("ship: released %s\n", tag)
	return nil
}

// repoSlug is owner/name: GITHUB_REPOSITORY in Actions, else from gh.
func repoSlug(root string) (string, error) {
	if s := os.Getenv("GITHUB_REPOSITORY"); s != "" {
		return s, nil
	}
	cmd := exec.Command("gh", "repo", "view", "--json", "nameWithOwner", "-q", ".nameWithOwner")
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gh repo view: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func run(dir string, dry bool, name string, args ...string) error {
	fmt.Printf("+ %s %s\n", name, strings.Join(args, " "))
	if dry {
		return nil
	}
	cmd := exec.Command(name, args...) //nolint:gosec // docker and gh with ship's own arguments
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}
