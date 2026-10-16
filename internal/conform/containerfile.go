package conform

import (
	"bufio"
	"bytes"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/trick77/ship/internal/compose"
	"github.com/trick77/ship/internal/pins"
)

// KitVersion is the -X target every image stamps its release version into.
const KitVersion = "github.com/trick77/ship/kit/version.Version"

// instr is one Containerfile instruction, continuation lines joined.
type instr struct {
	cmd  string // upper case
	args string
}

type stage struct {
	name   string // lower case, "" when unnamed
	base   string // the FROM image or stage name, flags dropped
	instrs []instr
}

// parseContainerfile splits a Containerfile into stages; ARGs before the first FROM go to global.
func parseContainerfile(b []byte) (global []instr, stages []stage) {
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	var cur strings.Builder
	flush := func() {
		line := strings.TrimSpace(cur.String())
		cur.Reset()
		if line == "" {
			return
		}
		cmd, args, _ := strings.Cut(line, " ")
		in := instr{cmd: strings.ToUpper(cmd), args: strings.TrimSpace(args)}
		if in.cmd != "FROM" {
			if len(stages) == 0 {
				global = append(global, in)
			} else {
				stages[len(stages)-1].instrs = append(stages[len(stages)-1].instrs, in)
			}
			return
		}
		var s stage
		f := strings.Fields(in.args)
		for len(f) > 0 && strings.HasPrefix(f[0], "--") {
			f = f[1:]
		}
		if len(f) > 0 {
			s.base = f[0]
		}
		if len(f) == 3 && strings.EqualFold(f[1], "as") {
			s.name = strings.ToLower(f[2])
		}
		stages = append(stages, s)
	}
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(l, "#") {
			continue
		}
		if c, ok := strings.CutSuffix(l, `\`); ok {
			cur.WriteString(c + " ")
			continue
		}
		cur.WriteString(l)
		flush()
	}
	flush()
	return global, stages
}

func stageIndex(stages []stage, ref string) int {
	ref = strings.ToLower(ref)
	if n, err := strconv.Atoi(ref); err == nil && n >= 0 && n < len(stages) {
		return n
	}
	return slices.IndexFunc(stages, func(s stage) bool { return s.name != "" && s.name == ref })
}

// closure is the final stage and every stage it builds on (FROM <stage>, COPY --from=<stage>).
func closure(stages []stage, final int) []int {
	seen := map[int]bool{}
	queue := []int{final}
	var out []int
	for len(queue) > 0 {
		i := queue[0]
		queue = queue[1:]
		if seen[i] {
			continue
		}
		seen[i] = true
		out = append(out, i)
		if j := stageIndex(stages[:i], stages[i].base); j >= 0 {
			queue = append(queue, j)
		}
		for _, in := range stages[i].instrs {
			if in.cmd != "COPY" {
				continue
			}
			for _, f := range strings.Fields(in.args) {
				if from, ok := strings.CutPrefix(f, "--from="); ok {
					if j := stageIndex(stages[:i], from); j >= 0 {
						queue = append(queue, j)
					}
				}
			}
		}
	}
	slices.Sort(out)
	return out
}

// chain is the final stage and the stages it is FROM, final first.
func chain(stages []stage, final int) []int {
	out := []int{final}
	for i := final; ; {
		j := stageIndex(stages[:i], stages[i].base)
		if j < 0 {
			return out
		}
		out = append(out, j)
		i = j
	}
}

var (
	// golangTag's third group is a patch the tag pins: golang:1.27.0-alpine, not golang:1.27-alpine.
	golangTag = regexp.MustCompile(`^(?:docker\.io/)?(?:library/)?golang:(\d+)\.(\d+)(\.\d+)?`)
	nodeTag   = regexp.MustCompile(`^(?:docker\.io/)?(?:library/)?node:(\d+)`)
	// toolchain matches any golang or node base, numbered or not: node:lts or golang:alpine would slip past the
	// floors and the go.mod match unseen.
	toolchain = regexp.MustCompile(`^(?:docker\.io/)?(?:library/)?(golang|node)(?::|@|$)`)
)

// nodeMajors are the node builder majors of the stages x is built from.
func (x image) nodeMajors() []string {
	var out []string
	for _, i := range closure(x.stages, x.final) {
		s := x.stages[i]
		if stageIndex(x.stages[:i], s.base) >= 0 {
			continue
		}
		if m := nodeTag.FindStringSubmatch(s.base); m != nil && !slices.Contains(out, m[1]) {
			out = append(out, m[1])
		}
	}
	return out
}

// nodeVersion is the Node major the images build the UI with, "" when none does: the CI caller sets up the same.
func (e *env) nodeVersion() string {
	for _, x := range e.images() {
		if x.err == "" {
			if n := x.nodeMajors(); len(n) > 0 {
				return n[0]
			}
		}
	}
	return ""
}

// image is one ship.yaml image's Containerfile, parsed, with its final stage.
type image struct {
	suffix string
	file   string
	global []instr
	stages []stage
	final  int
	err    string
}

func (e *env) images() []image {
	var out []image
	for _, im := range e.c.Images {
		x := image{suffix: im.Suffix, file: im.Containerfile}
		b, err := e.read(im.Containerfile)
		switch {
		case err != nil:
			x.err = err.Error()
		case b == nil:
			x.err = "missing"
		}
		if x.err == "" {
			x.global, x.stages = parseContainerfile(b)
			x.final = len(x.stages) - 1
			if im.Target != "" {
				x.final = stageIndex(x.stages, im.Target)
			}
			switch {
			case len(x.stages) == 0:
				x.err = "no FROM"
			case x.final < 0:
				x.err = fmt.Sprintf("no stage %q (ship.yaml images target)", im.Target)
			}
		}
		out = append(out, x)
	}
	return out
}

// containerfile: the release version stamped through kit, reproducible paths, a non-root runtime, the builder on
// go.mod's Go.
func (e *env) containerfile() result {
	if len(e.c.Images) == 0 {
		return result{skip: "no images"}
	}
	goMM, _ := e.goMajorMinor()
	var fs []finding
	for _, x := range e.images() {
		add := func(what string) {
			suffix := x.suffix
			fs = append(fs, finding{what: fmt.Sprintf("image %q: %s", x.suffix, what), file: x.file, image: &suffix})
		}
		if x.err != "" {
			add(x.err)
			continue
		}
		var runs []string
		// An ARG before the first FROM reaches no stage that does not redeclare it: the stage running go build
		// declares its own.
		global := slices.ContainsFunc(x.global, isVersionArg)
		argless := false
		var golang []string
		for _, i := range closure(x.stages, x.final) {
			s := x.stages[i]
			if stageIndex(x.stages[:i], s.base) < 0 {
				// Untagged is a floating :latest: no reproducible build, nothing Dependabot can track. A ${ARG}
				// base is the ARG's to pin.
				if s.base != "scratch" && !strings.Contains(s.base, "$") && !compose.Pinned(s.base) {
					add(fmt.Sprintf("base %s has no tag or digest", s.base))
				}
				if m := golangTag.FindStringSubmatchIndex(s.base); m != nil {
					golang = append(golang, s.base[m[2]:m[3]]+"."+s.base[m[4]:m[5]])
					// A patch tag freezes the stdlib the release image ships, while CI tests the newest patch. A
					// digest on the floating tag (golang:1.27-alpine@sha256:…) pins on purpose and Dependabot moves
					// it with every patch: it passes. A patch tag fails with or without a digest.
					if m[6] >= 0 {
						float, _, _ := strings.Cut(s.base[:m[6]]+s.base[m[7]:], "@")
						add(fmt.Sprintf("builder %s pins a patch: use %s, the floating tag gets the newest patch",
							s.base, float))
					}
				} else if t := toolchain.FindStringSubmatch(s.base); t != nil && !nodeTag.MatchString(s.base) {
					add(fmt.Sprintf("builder %s names no %s version", s.base, t[1]))
				}
			}
			arg := false
			for _, in := range s.instrs {
				arg = arg || isVersionArg(in)
				if in.cmd == "RUN" && strings.Contains(in.args, "go build") {
					runs = append(runs, in.args)
					argless = argless || !arg
				}
			}
		}
		switch {
		case argless && global:
			add("ARG VERSION before the first FROM reaches no stage: redeclare it in the stage running go build")
		case argless:
			add("no ARG VERSION in the stage running go build (ship release passes the version as a build arg)")
		}
		switch {
		case len(runs) == 0:
			add("no RUN go build in the stages the image is built from")
		default:
			if !slices.ContainsFunc(runs, func(r string) bool {
				return strings.Contains(r, "-X "+KitVersion+"=${VERSION}") || strings.Contains(r, "-X "+KitVersion+"=$VERSION")
			}) {
				add("go build ldflags lack -X " + KitVersion + "=${VERSION}")
			}
			if slices.ContainsFunc(runs, func(r string) bool { return !strings.Contains(r, "-trimpath") }) {
				add("go build without -trimpath")
			}
		}
		if !nonRoot(x.stages, x.final) {
			add("final stage runs as root: USER <non-root>, or a :nonroot distroless base")
		}
		for _, v := range golang {
			if goMM != "" && v != goMM {
				add(fmt.Sprintf("builder golang:%s, go.mod says go %s", v, goMM))
			}
			if !atLeast(v, pins.GoMin) {
				add(fmt.Sprintf("builder golang:%s, want at least %s (pins.GoMin)", v, pins.GoMin))
			}
		}
		for _, n := range x.nodeMajors() {
			if !atLeast(n, pins.NodeMin) {
				add(fmt.Sprintf("builder node:%s, want at least %s (pins.NodeMin)", n, pins.NodeMin))
			}
		}
	}
	return result{findings: fs}
}

func isVersionArg(in instr) bool {
	if in.cmd != "ARG" {
		return false
	}
	name, _, _ := strings.Cut(in.args, "=")
	return strings.TrimSpace(name) == "VERSION"
}

// nonRoot: the last USER of the final stage's chain names no root, or there is none and the base is :nonroot.
func nonRoot(stages []stage, final int) bool {
	ch := chain(stages, final)
	for _, i := range ch {
		for k := len(stages[i].instrs) - 1; k >= 0; k-- {
			if in := stages[i].instrs[k]; in.cmd == "USER" {
				u, _, _ := strings.Cut(in.args, ":")
				return u != "root" && u != "0" && u != ""
			}
		}
	}
	base, _, _ := strings.Cut(stages[ch[len(ch)-1]].base, "@")
	return strings.HasSuffix(base, ":nonroot") || strings.Contains(base, ":nonroot-")
}

// distroless: the runtime has no shell and no package manager to exploit.
func (e *env) distroless() result {
	if len(e.c.Images) == 0 {
		return result{skip: "no images"}
	}
	var fs []finding
	for _, x := range e.images() {
		if x.err != "" {
			continue // reported by containerfile
		}
		ch := chain(x.stages, x.final)
		if base := x.stages[ch[len(ch)-1]].base; !strings.HasPrefix(base, "gcr.io/distroless/") {
			suffix := x.suffix
			fs = append(fs, finding{what: fmt.Sprintf("image %q: final stage base %s, not gcr.io/distroless/", x.suffix,
				base), file: x.file, image: &suffix})
		}
	}
	return result{findings: fs}
}
