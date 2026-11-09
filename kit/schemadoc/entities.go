package schemadoc

import (
	"cmp"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// ModelFile is the file name Entities reads.
const ModelFile = "model.go"

// Entity is a struct declared in a model.go file, exported or not: a row type scanned in its own package is often
// unexported, and conform's model rule puts it in model.go with a Table: line all the same.
type Entity struct {
	// Package is the package name, Dir its directory relative to the walked root, slash-separated.
	Package string
	Dir     string
	Name    string
	// Doc is the type's doc comment without its Table: line.
	Doc string
	// Table is the table holding the type's rows, "-" for a type not stored as a row, empty when the doc names
	// none (an error at Render).
	Table  string
	Fields []Field
}

// Field is one field, exported or not (a scanned column may be unexported); blank fields are left out. Doc is its
// doc comment, else its trailing comment, whole, paragraphs split by a blank line. SharedDoc names the field whose
// doc comment heads the run of lines this one continues without one of its own, when that comment names it (// A,
// B and C are the clip's parts.\n A, B int\n C bool: B and C share A's); a comment naming only its head (// ID is
// the row id.) is its head's. Names on the commented line itself always share it (A, B int).
type Field struct {
	Name      string
	Type      string
	Doc       string
	SharedDoc string
	Embedded  bool
}

// NoTable is the Table value of a type not stored as a row.
const NoTable = "-"

var tableLine = regexp.MustCompile(`^Table:\s*(\S+)\s*$`)

// Entities reads every model.go below dir, skipping testdata, vendor and directories starting with "." or "_" (as
// the go tool does) and node_modules (a UI's packages, no Go of the module's), ordered by directory, then type name.
func Entities(dir string) ([]Entity, error) {
	var out []Entity
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			n := d.Name()
			skip := n == "testdata" || n == "vendor" || n == "node_modules" || strings.HasPrefix(n, ".") || strings.HasPrefix(n, "_")
			if path != dir && skip {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != ModelFile {
			return nil
		}
		rel, err := filepath.Rel(dir, filepath.Dir(path))
		if err != nil {
			return err
		}
		es, err := parseModel(path, filepath.ToSlash(rel))
		out = append(out, es...)
		return err
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b Entity) int { return cmp.Or(strings.Compare(a.Dir, b.Dir), strings.Compare(a.Name, b.Name)) })
	return out, nil
}

func parseModel(path, dir string) ([]Entity, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	var out []Entity
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			ts := spec.(*ast.TypeSpec)
			st, ok := ts.Type.(*ast.StructType)
			if !ok {
				continue
			}
			doc := ts.Doc
			if doc == nil && len(gen.Specs) == 1 {
				doc = gen.Doc
			}
			e := Entity{Package: f.Name.Name, Dir: dir, Name: ts.Name.Name, Fields: fields(fset, st)}
			e.Doc, e.Table = splitTable(doc.Text())
			out = append(out, e)
		}
	}
	return out, nil
}

// splitTable takes the Table: line out of a doc comment.
func splitTable(doc string) (string, string) {
	var keep []string
	table := ""
	for _, line := range strings.Split(doc, "\n") {
		if m := tableLine.FindStringSubmatch(line); m != nil {
			table = m[1]
			continue
		}
		keep = append(keep, line)
	}
	return strings.TrimSpace(strings.Join(keep, "\n")), table
}

func fields(fset *token.FileSet, st *ast.StructType) []Field {
	var out []Field
	// The field heading the current run of lines and its doc, the run's last line.
	shared, sharedDoc, prevEnd := "", "", -1
	for _, f := range st.Fields.List {
		typ := types.ExprString(f.Type)
		doc := paragraphs(f.Doc.Text())
		ownDoc := doc != "" // a directive alone (//nolint:…) is no doc
		if !ownDoc {
			doc = paragraphs(f.Comment.Text())
		}
		start, end := fset.Position(f.Pos()).Line, fset.Position(f.End()).Line
		if ownDoc || f.Comment != nil || start != prevEnd+1 {
			shared = "" // its own comment, or a blank line, ends the run; a doc starts a new one below
		}
		prevEnd = end
		names := make([]string, 0, len(f.Names))
		for _, n := range f.Names {
			if n.Name != "_" {
				names = append(names, n.Name)
			}
		}
		embedded := len(f.Names) == 0
		if embedded {
			name := typ[strings.LastIndex(typ, ".")+1:]
			names = []string{strings.TrimPrefix(name, "*")}
		}
		for i, n := range names {
			fd := Field{Name: n, Type: typ, Doc: doc, Embedded: embedded}
			switch {
			case ownDoc && i > 0:
				fd.Doc, fd.SharedDoc = "", names[0] // A, B int under one comment: B shares A's
			case doc == "" && shared != "" && mentions(sharedDoc, n):
				fd.SharedDoc = shared
			}
			out = append(out, fd)
		}
		if ownDoc && len(names) > 0 {
			shared, sharedDoc = names[0], doc
		}
	}
	return out
}

// mentions reports whether doc names the word w as a word of its own.
func mentions(doc, w string) bool {
	for i := 0; ; {
		j := strings.Index(doc[i:], w)
		if j < 0 {
			return false
		}
		j += i
		if (j == 0 || !identByte(doc[j-1])) && (j+len(w) == len(doc) || !identByte(doc[j+len(w)])) {
			return true
		}
		i = j + 1
	}
}

// paragraphs is s with each paragraph on one line, paragraphs split by a blank line.
func paragraphs(s string) string {
	var out []string
	for p := range strings.SplitSeq(s, "\n\n") {
		if p = oneLine(p); p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "\n\n")
}

// Summary is the doc's first paragraph on one line.
func (e Entity) Summary() string {
	para, _, _ := strings.Cut(e.Doc, "\n\n")
	return oneLine(para)
}

// QualifiedName is package.Type.
func (e Entity) QualifiedName() string { return e.Package + "." + e.Name }
