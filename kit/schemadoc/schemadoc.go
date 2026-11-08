// Package schemadoc writes a repo's database schema as Markdown: one section per table (columns, foreign keys,
// indexes, CHECK constraints and the Go types that hold its rows), the views by name, and a Mermaid erDiagram, which
// GitHub renders.
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
// rewrites the file instead (see Regenerate; go test runs in the Go module directory, not a repo root above it).
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

// Dialect reads one engine's catalog.
type Dialect struct {
	describe func(ctx context.Context, db *sql.DB) (Schema, error)
}

// SQLite reads sqlite_master and the table-valued pragmas, and the CREATE statements for what those leave out (an
// index expression's text, CHECK constraints). A virtual table is listed once, with its module and declared
// columns; its shadow tables are not (see isShadow).
var SQLite = Dialect{describe: describeSQLite}

// Postgres reads pg_catalog for the tables and views of current_schema().
var Postgres = Dialect{describe: describePostgres}

// skipped are the migration bookkeeping tables: kit/migrate's and goose's, which a converted database keeps.
var skipped = []string{migrate.Table, migrate.GooseTable}

// Describe reads db's schema. The migration bookkeeping tables are left out.
func Describe(ctx context.Context, db *sql.DB, d Dialect) (Schema, error) {
	s, err := d.describe(ctx, db)
	if err != nil {
		return Schema{}, err
	}
	s.sort()
	return s, nil
}

// sort orders s's tables and views by name.
func (s *Schema) sort() {
	slices.SortFunc(s.Tables, func(a, b Table) int { return strings.Compare(a.Name, b.Name) })
	slices.SortFunc(s.Views, func(a, b View) int { return strings.Compare(a.Name, b.Name) })
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
