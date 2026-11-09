package schemadoc

import (
	"regexp"
	"slices"
	"strings"
)

// Schema is the tables and views of a database, each ordered by name.
//
// Table: -
type Schema struct {
	Tables []Table
	Views  []View
}

// View is a view, by name and Kind ("view", "materialized view", "continuous aggregate": TimescaleDB's, a view to
// Postgres) only: its definition is the engine's rewrite of the query, which changes with the engine's version.
//
// Table: -
type View struct {
	Name string
	Kind string
}

// table is the table named name, nil when there is none.
func (s Schema) table(name string) *Table {
	for i := range s.Tables {
		if s.Tables[i].Name == name {
			return &s.Tables[i]
		}
	}
	return nil
}

// hasView reports whether s has a view named name.
func (s Schema) hasView(name string) bool {
	return slices.ContainsFunc(s.Views, func(v View) bool { return v.Name == name })
}

// Table is one table. Module names a virtual table's module (vec0, fts5), empty for a plain table; ModuleArgs is
// what its CREATE statement passed the module, on one line. A module may declare its columns untyped (vec0 does), so
// the arguments are where a vector's width shows. Checks is the CHECK constraints, each CHECK (<expression>) as the
// engine spells it (SQLite as written, Postgres as pg_get_constraintdef rewrites it), prefixed CONSTRAINT <name>
// where named: always in Postgres, which names every one.
//
// Table: -
type Table struct {
	Name        string
	Module      string
	ModuleArgs  string
	Columns     []Column
	ForeignKeys []ForeignKey
	Indexes     []Index
	Checks      []string
}

// pkColumns is t's primary key columns in key order.
func (t Table) pkColumns() []string {
	var pk []Column
	for _, c := range t.Columns {
		if c.PK > 0 {
			pk = append(pk, c)
		}
	}
	slices.SortFunc(pk, func(a, b Column) int { return a.PK - b.PK })
	var out []string
	for _, c := range pk {
		out = append(out, c.Name)
	}
	return out
}

// oneToOne reports whether cols hold all of t's primary key or of a unique index without a predicate (a partial one
// is unique only where its predicate holds) or whose predicate only keeps its keys non-NULL: then at most one row
// carries any one value of cols.
func (t Table) oneToOne(cols []string) bool {
	covers := func(keys []string) bool {
		if len(keys) == 0 {
			return false
		}
		for _, k := range keys {
			if !slices.Contains(cols, k) {
				return false
			}
		}
		return true
	}
	if covers(t.pkColumns()) {
		return true
	}
	return slices.ContainsFunc(t.Indexes, func(i Index) bool {
		return i.Unique && nullGuard(i.Where, i.Keys) && !slices.Contains(i.Keys, "") && covers(i.Keys)
	})
}

// nullGuard reports whether partial index predicate where leaves the index as unique as none: it is empty, or only
// AND-joined `<key> IS NOT NULL` terms, each column one of keys. Rows it leaves out hold a NULL key, which never
// collides in a unique index anyway. A quoted column matches its key exactly (Postgres folds only bare names), a
// bare one in any case.
func nullGuard(where string, keys []string) bool {
	if where == "" {
		return true
	}
	for _, term := range andTerms(where) {
		col, quoted, ok := notNullColumn(term)
		if !ok || !slices.ContainsFunc(keys, func(k string) bool { return k == col || !quoted && strings.EqualFold(k, col) }) {
			return false
		}
	}
	return true
}

// andTerms is predicate where split at its top-level AND keywords (outside literals and quoted names), each term
// trimmed and its parentheses turned to spaces: with only AND joining, grouping changes nothing, and a term holding
// any other operator fails notNullColumn.
func andTerms(where string) []string {
	var terms []string
	var b strings.Builder
	for i := 0; i < len(where); {
		if j := skipQuoted(where, i); j != i {
			b.WriteString(where[i:j])
			i = j
			continue
		}
		switch {
		case where[i] == '(' || where[i] == ')':
			b.WriteByte(' ')
			i++
		case keywordAt(where, i, "AND"):
			terms = append(terms, strings.TrimSpace(b.String()))
			b.Reset()
			i += len("AND")
		default:
			b.WriteByte(where[i])
			i++
		}
	}
	return append(terms, strings.TrimSpace(b.String()))
}

// notNullTail is what follows the column in `<column> IS NOT NULL` (or SQLite's NOT NULL, NOTNULL).
var notNullTail = regexp.MustCompile(`(?i)^\s+(?:IS\s+NOT\s+NULL|NOT\s+NULL|NOTNULL)$`)

// notNullColumn is the column, unquoted, of a `<column> IS NOT NULL` term, and whether it was quoted ("x", `x`,
// [x]); ok is false for any other term.
func notNullColumn(term string) (col string, quoted, ok bool) {
	end := 0
	if term != "" && strings.IndexByte("\"`[", term[0]) >= 0 {
		end, quoted = skipQuoted(term, 0), true
	} else {
		for end < len(term) && identByte(term[end]) {
			end++
		}
	}
	if end == 0 || !notNullTail.MatchString(term[end:]) {
		return "", false, false
	}
	return leadingName(term[:end]), quoted, true
}

// Column is one column. PK is its 1-based position in the primary key, 0 when not part of it. Default is the
// default's SQL text, meaningful only when HasDefault; a Postgres identity column's is "identity always" or
// "identity by default", a generated column's "generated: <expression>".
//
// Table: -
type Column struct {
	Name       string
	Type       string
	NotNull    bool
	Default    string
	HasDefault bool
	PK         int
}

// ForeignKey is one, possibly composite, foreign key. OnUpdate and OnDelete are spelled as in SQL (NO ACTION,
// CASCADE, SET NULL, SET DEFAULT, RESTRICT).
//
// Table: -
type ForeignKey struct {
	Columns    []string
	RefTable   string
	RefColumns []string
	OnUpdate   string
	OnDelete   string
}

// Index is one secondary index. A primary key's own index is left out; a UNIQUE constraint's is kept. Columns are
// key columns in order, an expression as its text; SQLite appends " COLLATE <name>" where not BINARY, both engines
// " DESC" where descending. Keys is the same keys' bare column names, "" for an expression: what a foreign key
// matches. Where is a partial index's predicate.
//
// Table: -
type Index struct {
	Name    string
	Columns []string
	Keys    []string
	Unique  bool
	Where   string
}
