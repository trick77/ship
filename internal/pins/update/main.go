// Command update moves internal/pins to the newest releases (the Pins workflow, daily) and prints the commit
// subject, nothing when every pin is current. Run from the repo root. A pin whose lookup fails is a warning while
// others moved (they land), a failure when none did.
package main

import (
	"fmt"
	"io"
	"os"
	"strings"
)

func main() { os.Exit(run(".", httpGet, os.Stdout, os.Stderr)) }

func run(root string, get Get, stdout, stderr io.Writer) int {
	cs, err := Update(root, get)
	for _, c := range cs {
		_, _ = fmt.Fprintf(stderr, "%s %s -> %s\n", c.Name, c.From, c.To)
	}
	if err != nil {
		level := "error"
		if len(cs) > 0 {
			level = "warning"
		}
		_, _ = fmt.Fprintf(stderr, "::%s::pins: %s\n", level, strings.ReplaceAll(err.Error(), "\n", "; "))
		if len(cs) == 0 {
			return 1
		}
	}
	if len(cs) > 0 {
		_, _ = fmt.Fprintln(stdout, Subject(cs))
	}
	return 0
}
