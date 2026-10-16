package conform

import (
	"strings"
	"testing"
)

const testDigest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// CI builds with the newest patch of go.mod's Go; a builder tag pinning a patch ships an older stdlib in the image.
func TestContainerfile_whenGolangPatchPinned_thenFails(t *testing.T) {
	for _, tc := range []struct{ tag, want string }{
		{"golang:1.27.0-alpine", "builder golang:1.27.0-alpine pins a patch: use golang:1.27-alpine, the floating tag gets the newest patch"},
		{"golang:1.27.12", "builder golang:1.27.12 pins a patch: use golang:1.27, the floating tag gets the newest patch"},
		{"docker.io/library/golang:1.27.0-alpine3.22",
			"builder docker.io/library/golang:1.27.0-alpine3.22 pins a patch: use docker.io/library/golang:1.27-alpine3.22"},
		// The digest pins the patch's image all the same; the hint drops the stale digest, asks for the new one.
		{"golang:1.27.0-alpine@sha256:" + testDigest, "builder golang:1.27.0-alpine@sha256:" + testDigest +
			" pins a patch: use golang:1.27-alpine (its own digest), the floating tag"},
	} {
		t.Run(tc.tag, func(t *testing.T) {
			r := app(t)
			r.edit("Containerfile", "golang:1.27-alpine", tc.tag)
			out, err := r.run(appShip)
			if err == nil {
				t.Fatalf("passed:\n%s", out)
			}
			// Both images build from the stage.
			for _, im := range []string{`""`, `"-ui"`} {
				if !strings.Contains(out, "FAIL containerfile: image "+im+": "+tc.want) {
					t.Errorf("no %s finding for %s in:\n%s", tc.want, im, out)
				}
			}
			if strings.Contains(out, "go.mod says") {
				t.Errorf("major.minor matches go.mod:\n%s", out)
			}
		})
	}
}

func TestContainerfile_whenGolangFloatsOrDigest_thenOK(t *testing.T) {
	for _, tag := range []string{"golang:1.27", "golang:1.27-alpine3.22",
		// A digest on the floating tag pins on purpose and Dependabot moves it with every patch.
		"golang:1.27-alpine@sha256:" + testDigest} {
		t.Run(tag, func(t *testing.T) {
			r := app(t)
			r.edit("Containerfile", "golang:1.27-alpine", tag)
			if out, err := r.run(appShip); err != nil || !strings.Contains(out, "ok containerfile\n") {
				t.Errorf("%v\n%s", err, out)
			}
		})
	}
}

func TestContainerfile_whenGolangPatchAllowed_thenWhyPrinted(t *testing.T) {
	r := app(t)
	r.edit("Containerfile", "golang:1.27-alpine", "golang:1.27.0-alpine")
	out, err := r.run(appShip + "allow:\n  - { rule: containerfile, why: stdlib regression in 1.27.1 }\n")
	if err != nil || !strings.Contains(out, "allowed containerfile: stdlib regression in 1.27.1\n") || strings.Contains(out, "FAIL") {
		t.Errorf("%v\n%s", err, out)
	}
}
