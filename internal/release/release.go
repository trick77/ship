// Package release cuts a version: the next semver tag claimed, images built and pushed, the GitHub release, then
// the moving image tags.
//
// Creating the tag is atomic, so it claims the version against a concurrent release; a failed build deletes it
// again, so no orphan tag burns a number (the next version derives from the newest tag on origin). :latest and
// friends move only once the release exists. Tag and release go through the GitHub API (gh): no git push
// credentials needed.
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
	// ghcr images are built on GitHub-hosted runners only: never a laptop, never a self-hosted runner (both can set
	// GITHUB_ACTIONS). Checked before anything, the tag claim included.
	if len(c.Images) > 0 && !o.DryRun && os.Getenv("RUNNER_ENVIRONMENT") != "github-hosted" {
		return fmt.Errorf("images are built on GitHub-hosted runners only: run the repo's Release workflow (or -dry-run here)")
	}
	// The branch only: tags come from ls-remote, and fetching them would fail on a local tag that differs from origin.
	if _, err := gitx.Git(root, "fetch", "-q", "--no-tags", "origin", c.Base); err != nil {
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
	// Tags as origin has them: a local-only or since-deleted tag must neither burn a number nor block a release.
	tags, atHead, err := remoteTags(root, head)
	if err != nil {
		return err
	}
	// A release that died after its tag (GitHub release or moving tags failed) must be finished, not skipped: a
	// rerun would otherwise move on to the next version and leave this one half done for good.
	if v := Latest(atHead, c.TagPrefix); v != (Version{}) {
		t := c.TagPrefix + v.String()
		return fmt.Errorf("HEAD is already tagged %s on origin: nothing new to release. If that release is "+
			"incomplete: with its images pushed, finish it (gh release create %s, then docker buildx imagetools "+
			"create the moving tags); without them (a killed build), give the number back and rerun: "+
			"gh api -X DELETE repos/<owner>/<repo>/git/refs/tags/%s", t, t, t)
	}
	prev := Latest(tags, c.TagPrefix)
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

	// The tag first: creating a ref is atomic, so it claims the version. A second release racing for the same
	// number fails here, before it could push its build over this version's image. A failed build gives the
	// number back by deleting the tag again.
	tagAPI := "repos/" + repo + "/git/refs"
	if err := run(root, o.DryRun, "gh", "api", tagAPI, "-f", "ref=refs/tags/"+tag, "-f", "sha="+head); err != nil {
		return fmt.Errorf("claim tag %s (taken by a concurrent release?): %w", tag, err)
	}
	unclaim := func(cause error) error {
		if err := run(root, o.DryRun, "gh", "api", "-X", "DELETE", tagAPI+"/tags/"+tag); err != nil {
			return fmt.Errorf("%w; deleting tag %s failed too, delete it by hand: %w", cause, tag, err)
		}
		return cause
	}

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
			// One cache scope per image: with the default scope the second image's export replaces the first's.
			scope := ",scope=" + name
			args = append(args, "--cache-from", "type="+o.Cache+scope, "--cache-to", "type="+o.Cache+scope+",mode=max")
		}
		for _, t := range Fixed(name, next, short) {
			args = append(args, "-t", t)
		}
		args = append(args, im.Context)
		if err := run(root, o.DryRun, "docker", args...); err != nil {
			return unclaim(fmt.Errorf("image %s: %w", name, err))
		}
		images = append(images, fmt.Sprintf("`%s:%s`", name, next))
		names = append(names, name)
	}

	notes := "Release " + next.String()
	if len(images) > 0 {
		notes = "Container images: " + strings.Join(images, ", ")
	}
	relArgs := []string{"release", "create", tag, "--repo", repo, "--title", tag, "--notes", notes}
	if err := run(root, o.DryRun, "gh", relArgs...); err != nil {
		return fmt.Errorf("github release (tag %s exists; finish by hand: gh %s, then the moving tags): %w",
			tag, strings.Join(relArgs, " "), err)
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

// remoteTags lists origin's tag names, and those pointing at head (an annotated tag via its peeled ^{} line).
func remoteTags(root, head string) (all, atHead []string, err error) {
	out, err := gitx.Git(root, "ls-remote", "--tags", "origin")
	if err != nil {
		return nil, nil, err
	}
	for _, line := range strings.Split(out, "\n") {
		sha, ref, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		name := strings.TrimPrefix(ref, "refs/tags/")
		peeled := strings.HasSuffix(name, "^{}")
		name = strings.TrimSuffix(name, "^{}")
		if !peeled {
			all = append(all, name)
		}
		if sha == head {
			atHead = append(atHead, name)
		}
	}
	return all, atHead, nil
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
