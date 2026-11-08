// Package schemadoc writes a repo's database schema as Markdown: one section per table (columns, foreign keys,
// indexes and the Go types that hold its rows) and a Mermaid erDiagram, which GitHub renders.
//
// The doc is generated, never written by hand, and checked like a golden file by the consumer's own test: it opens a
// fresh database with its own driver and extensions, runs its migrations, and compares
//
//	s, err := schemadoc.Describe(ctx, db, schemadoc.SQLite)
//	es, err := schemadoc.Entities("../..")    // the module root, from the test's directory
//	out, err := schemadoc.Render(s, es)
//	schemadoc.Golden(t, out, "../../docs/schema.md")
//
// against the committed file, so a migration or model change without its doc fails the gate. SCHEMADOC_UPDATE=1
// rewrites the file instead.
//
// Go types are found by convention: structs, exported or not, in files named model.go whose doc comment ends in a line
// "Table: <name>" (the table holding their rows) or "Table: -" (not stored as a row). A struct without that line, or
// naming a table the schema lacks, fails Render: the mapping is the part that drifts.
package schemadoc

import (
	"context"
	"database/sql"
	"slices"
	"strings"

	"github.com/trick77/ship/kit/migrate"
)

// Schema is the tables of a database, ordered by name.
type Schema struct {
	Tables []Table
}

// Table is one table. Module names a virtual table's module (vec0, fts5), empty for a plain table; ModuleArgs is
// what its CREATE statement passed the module, on one line. A module may declare its columns untyped (vec0 does), so
// the arguments are where a vector's width shows.
type Table struct {
	Name        string
	Module      string
	ModuleArgs  string
	Columns     []Column
	ForeignKeys []ForeignKey
	Indexes     []Index
}

// Column is one column. PK is its 1-based position in the primary key, 0 when not part of it. Default is the
// default's SQL text, meaningful only when HasDefault.
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
type ForeignKey struct {
	Columns    []string
	RefTable   string
	RefColumns []string
	OnUpdate   string
	OnDelete   string
}

// Index is one secondary index. A primary key's own index is left out; a UNIQUE constraint's is kept. Columns are
// key columns in order, with " DESC" appended where descending; Where is a partial index's predicate.
type Index struct {
	Name    string
	Columns []string
	Unique  bool
	Where   string
}

// Dialect reads one engine's catalog.
type Dialect struct {
	describe func(ctx context.Context, db *sql.DB) ([]Table, error)
}

// SQLite reads sqlite_master and the table-valued pragmas. A virtual table is listed once, with its module and
// declared columns; its shadow tables are not (see isShadow).
var SQLite = Dialect{describe: describeSQLite}

// Postgres reads pg_catalog for the tables of current_schema().
var Postgres = Dialect{describe: describePostgres}

// skipped are the migration bookkeeping tables: kit/migrate's and goose's, which a converted database keeps.
var skipped = []string{migrate.Table, migrate.GooseTable}

// Describe reads db's schema. The migration bookkeeping tables are left out.
func Describe(ctx context.Context, db *sql.DB, d Dialect) (Schema, error) {
	tables, err := d.describe(ctx, db)
	if err != nil {
		return Schema{}, err
	}
	slices.SortFunc(tables, func(a, b Table) int { return strings.Compare(a.Name, b.Name) })
	return Schema{Tables: tables}, nil
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

// queryRows runs q and calls scan for each row.
func queryRows(ctx context.Context, db *sql.DB, q string, scan func(*sql.Rows) error, args ...any) error {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}
