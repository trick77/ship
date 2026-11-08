// Command update moves internal/pins to the newest releases (the Pins workflow, daily) and prints the commit
// subject, nothing when every pin is current. Run from the repo root.
package main

import (
	"fmt"
	"os"

	"github.com/trick77/ship/internal/pins"
)

func main() {
	cs, err := pins.Update(".", pins.HTTPGet)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pins:", err)
		os.Exit(1)
	}
	if len(cs) > 0 {
		fmt.Println(pins.Subject(cs))
	}
	for _, c := range cs {
		fmt.Fprintf(os.Stderr, "%s %s -> %s\n", c.Name, c.From, c.To)
	}
}
