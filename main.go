// Command ship is the gate, hook and release tool shared by the trick77 and ipverse Go repos. Each repo runs the
// newest one, master's head (go run github.com/trick77/ship@<master's sha> <command>), and describes itself in
// ship.yaml.
//
//	ship check                     everything CI checks, locally
//	ship lint                      golangci-lint alone, at the version check runs
//	ship conform [-fix]            the repo against ship's canon
//	ship init [-diff]              write the files ship owns
//	ship hooks                     install the pre-push hook
//	ship pre-push                  the hook's gate (git runs it)
//	ship release [patch|minor|major|auto] [-cache gha] [-dry-run] [-dir kit]
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/trick77/ship/internal/check"
	"github.com/trick77/ship/internal/config"
	"github.com/trick77/ship/internal/conform"
	"github.com/trick77/ship/internal/gitx"
	"github.com/trick77/ship/internal/hook"
	"github.com/trick77/ship/internal/release"
)

const usage = `usage: ship <command>

  check                                       run the gate: everything CI checks, locally
  lint                                        golangci-lint alone, at the version check runs
  conform [-fix]                              the repo against ship's canon (check's first step); -fix: init first
  init [-diff]                                write the files ship owns; -diff: print, write nothing, exit 1 if any
  hooks                                      install the pre-push hook running the gate
  pre-push                                    the hook's gate (git runs it)
  release [patch|minor|major|auto] [flags]    tag, build and push images, GitHub release; auto: patch, or what
                                              the commits since the last tag ask for (Release: minor|major)
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "ship:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("no command")
	}
	if h := args[0]; h == "-h" || h == "-help" || h == "--help" || h == "help" {
		fmt.Print(usage)
		return nil
	}
	// git gives a linked worktree's hooks GIT_DIR without GIT_WORK_TREE; a hook from ship v0.2.0 or one wired by
	// hand then runs ship from the module dir, where git would take that dir as the top of the tree.
	if err := gitx.PinWorkTree(); err != nil {
		return err
	}
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	root, err := gitx.Root(wd)
	if err != nil {
		return err
	}
	switch args[0] {
	case "check":
		c, err := config.Load(root)
		if err != nil {
			return err
		}
		return check.Run(root, c)
	case "lint":
		c, err := config.Load(root)
		if err != nil {
			return err
		}
		return check.Lint(root, c)
	case "conform", "init":
		return conformCmd(root, args[0], args[1:])
	case "hooks":
		p, err := hook.Install(root)
		if err != nil {
			return err
		}
		fmt.Println("ship: pre-push hook installed at", p)
		return nil
	case "pre-push":
		refs, err := hook.ParseRefs(os.Stdin)
		if err != nil {
			return err
		}
		gated := hook.Gated(refs)
		if len(gated) == 0 {
			return nil
		}
		if hook.Stale(root) {
			fmt.Fprintln(os.Stderr, "ship: this clone's pre-push hook is not this ship's script; rerun `ship hooks` (see README)")
		}
		if err := hook.Ready(root, gated); err != nil {
			return fmt.Errorf("pre-push: %w", err)
		}
		c, err := config.Load(root)
		if err != nil {
			return err
		}
		return check.Run(root, c)
	case "release":
		o, err := releaseOptions(args[1:])
		if err != nil {
			return err
		}
		c, err := config.Load(root)
		if err != nil {
			return err
		}
		return release.Run(root, c, o)
	}
	fmt.Fprint(os.Stderr, usage)
	return fmt.Errorf("unknown command %q", args[0])
}

// conformCmd runs `conform [-fix]` or `init [-diff]`.
func conformCmd(root, name string, args []string) error {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fix := fs.Bool("fix", false, "write the files ship owns, then conform: what is left is by hand")
	diff := fs.Bool("diff", false, "print what init would write, write nothing; exit 1 when anything differs")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 || name == "conform" && *diff || name == "init" && *fix {
		return fmt.Errorf("%s: unexpected arguments %v", name, args)
	}
	c, err := config.Load(root)
	if err != nil {
		return err
	}
	switch {
	case name == "init":
		return conform.Init(root, c, conform.Options{}, *diff)
	case *fix:
		return conform.Fix(root, c, conform.Options{})
	}
	return conform.Run(root, c, conform.Options{})
}

// releaseOptions parses `release [bump] [flags] [bump]`: the bump may come before or after the flags (default
// patch). A second bump or any other argument is an error, never silently a patch.
func releaseOptions(args []string) (release.Options, error) {
	fs := flag.NewFlagSet("release", flag.ContinueOnError)
	cache := fs.String("cache", "", "buildx cache backend type, e.g. gha")
	dry := fs.Bool("dry-run", false, "print the commands, run none")
	dir := fs.String("dir", "", "release the nested Go module at this repo-relative dir (tags <dir>/vX.Y.Z)")
	bump := ""
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		bump, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return release.Options{}, err
	}
	switch {
	case fs.NArg() == 1 && bump == "":
		bump = fs.Arg(0)
	case fs.NArg() > 0:
		return release.Options{}, fmt.Errorf("release: unexpected arguments %v", fs.Args())
	case bump == "":
		bump = "patch"
	}
	return release.Options{Bump: bump, Cache: *cache, DryRun: *dry, Dir: *dir}, nil
}
