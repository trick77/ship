// Command ship is the gate, hook and release tool shared by the trick77 and ipverse Go repos. Each repo pins it
// as a go tool (go get -tool github.com/trick77/ship@vX) and describes itself in ship.yaml.
//
//	go tool ship check                     everything CI checks, locally
//	go tool ship hooks                     install the pre-push hook
//	go tool ship pre-push                  the hook's gate (git runs it)
//	go tool ship release [patch|minor|major] [-cache gha] [-dry-run]
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/trick77/ship/internal/check"
	"github.com/trick77/ship/internal/config"
	"github.com/trick77/ship/internal/gitx"
	"github.com/trick77/ship/internal/hook"
	"github.com/trick77/ship/internal/release"
)

const usage = `usage: ship <command>

  check                                  run the gate: everything CI checks, locally
  hooks                                  install the pre-push hook running the gate
  pre-push                               the hook's gate (git runs it)
  release [patch|minor|major] [flags]    tag, build and push images, GitHub release
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
	if args[0] == "pre-push" {
		gitx.UnsetRepoVars() // git's hook environment; hooks installed by ship v0.2.0 do not unset it themselves
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

// releaseOptions parses `release [bump] [flags] [bump]`: the bump may come before or after the flags (default
// patch). A second bump or any other argument is an error, never silently a patch.
func releaseOptions(args []string) (release.Options, error) {
	fs := flag.NewFlagSet("release", flag.ContinueOnError)
	cache := fs.String("cache", "", "buildx cache backend type, e.g. gha")
	dry := fs.Bool("dry-run", false, "print the commands, run none")
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
	return release.Options{Bump: bump, Cache: *cache, DryRun: *dry}, nil
}
