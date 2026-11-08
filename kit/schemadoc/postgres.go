package schemadoc

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
)

// The catalog queries, all on current_schema(). A table is $1, its regclass built with quote_ident so mixed-case
// names resolve. Column lists come back as one string joined by U+001F (no array support needed from the driver;
// an index expression may hold commas, never that). A relation an extension owns (PostGIS' spatial_ref_sys and
// geometry_columns, pg_stat_statements' views, in the schema it was created in) is the extension's, not the schema's:
// left out, or the doc would change with the extension's version.
const (
	qPostgresTables = `SELECT c.relname FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = current_schema() AND c.relkind IN ('r', 'p') AND NOT c.relispartition AND ` + pgNotExtension + `
		ORDER BY c.relname`
	qPostgresViews = `SELECT c.relname, c.relkind::text FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = current_schema() AND c.relkind IN ('v', 'm') AND ` + pgNotExtension + `
		ORDER BY c.relname`
	pgNotExtension = `NOT EXISTS (SELECT 1 FROM pg_depend dep
			WHERE dep.classid = 'pg_class'::regclass AND dep.objid = c.oid AND dep.deptype = 'e')`
	qPostgresColumns = `SELECT a.attname, format_type(a.atttypid, a.atttypmod), a.attnotnull,
			pg_get_expr(d.adbin, d.adrelid), a.attidentity::text, a.attgenerated::text,
			COALESCE((SELECT k.ordinal_position FROM information_schema.table_constraints tc
				JOIN information_schema.key_column_usage k
					ON k.constraint_schema = tc.constraint_schema AND k.constraint_name = tc.constraint_name
				WHERE tc.table_schema = current_schema() AND tc.table_name = $1
					AND tc.constraint_type = 'PRIMARY KEY' AND k.column_name = a.attname), 0)
		FROM pg_attribute a LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		WHERE a.attrelid = to_regclass(quote_ident(current_schema()) || '.' || quote_ident($1))
			AND a.attnum > 0 AND NOT a.attisdropped
		ORDER BY a.attnum`
	qPostgresForeignKeys = `SELECT con.conname, ref.relname,
			array_to_string(ARRAY(SELECT a.attname FROM unnest(con.conkey) WITH ORDINALITY AS k(n, o)
				JOIN pg_attribute a ON a.attrelid = con.conrelid AND a.attnum = k.n ORDER BY k.o), chr(31)),
			array_to_string(ARRAY(SELECT a.attname FROM unnest(con.confkey) WITH ORDINALITY AS k(n, o)
				JOIN pg_attribute a ON a.attrelid = con.confrelid AND a.attnum = k.n ORDER BY k.o), chr(31)),
			con.confupdtype::text, con.confdeltype::text
		FROM pg_constraint con JOIN pg_class ref ON ref.oid = con.confrelid
		WHERE con.contype = 'f' AND con.conrelid = to_regclass(quote_ident(current_schema()) || '.' || quote_ident($1))
		ORDER BY con.conname`
	qPostgresIndexes = `SELECT ic.relname, ix.indisunique,
			array_to_string(ARRAY(SELECT pg_get_indexdef(ix.indexrelid, g, true)
				FROM generate_series(1, ix.indnkeyatts) AS g ORDER BY g), chr(31)),
			COALESCE(pg_get_expr(ix.indpred, ix.indrelid, true), '')
		FROM pg_index ix JOIN pg_class ic ON ic.oid = ix.indexrelid
		WHERE ix.indrelid = to_regclass(quote_ident(current_schema()) || '.' || quote_ident($1))
			AND NOT ix.indisprimary
		ORDER BY ic.relname`
	qPostgresChecks = `SELECT pg_get_constraintdef(con.oid, true) FROM pg_constraint con
		WHERE con.contype = 'c' AND con.conrelid = to_regclass(quote_ident(current_schema()) || '.' || quote_ident($1))
		ORDER BY con.conname`
)

const pgSep = "\x1f"

func describePostgres(ctx context.Context, db *sql.DB) (Schema, error) {
	var names []string
	err := queryRows(ctx, db, qPostgresTables, func(r *sql.Rows) error {
		var n string
		if err := r.Scan(&n); err != nil {
			return err
		}
		names = append(names, n)
		return nil
	})
	if err != nil {
		return Schema{}, fmt.Errorf("list tables: %w", err)
	}
	var s Schema
	for _, n := range names {
		if slices.Contains(skipped, n) {
			continue
		}
		t, err := describePostgresTable(ctx, db, n)
		if err != nil {
			return Schema{}, fmt.Errorf("describe %s: %w", n, err)
		}
		s.Tables = append(s.Tables, t)
	}
	err = queryRows(ctx, db, qPostgresViews, func(r *sql.Rows) error {
		var v View
		if err := r.Scan(&v.Name, &v.Kind); err != nil {
			return err
		}
		if v.Kind == "m" {
			v.Kind = "materialized view"
		} else {
			v.Kind = "view"
		}
		s.Views = append(s.Views, v)
		return nil
	})
	if err != nil {
		return Schema{}, fmt.Errorf("list views: %w", err)
	}
	return s, nil
}

func describePostgresTable(ctx context.Context, db *sql.DB, name string) (Table, error) {
	t := Table{Name: name}
	err := queryRows(ctx, db, qPostgresColumns, func(r *sql.Rows) error {
		var c Column
		var def sql.NullString
		var identity, generated string
		if err := r.Scan(&c.Name, &c.Type, &c.NotNull, &def, &identity, &generated, &c.PK); err != nil {
			return err
		}
		c.Default, c.HasDefault = pgDefault(def, identity, generated)
		t.Columns = append(t.Columns, c)
		return nil
	}, name)
	if err != nil {
		return t, err
	}
	err = queryRows(ctx, db, qPostgresForeignKeys, func(r *sql.Rows) error {
		var conname, cols, refCols, onUpdate, onDelete string
		fk := ForeignKey{}
		if err := r.Scan(&conname, &fk.RefTable, &cols, &refCols, &onUpdate, &onDelete); err != nil {
			return err
		}
		fk.Columns, fk.RefColumns = strings.Split(cols, pgSep), strings.Split(refCols, pgSep)
		fk.OnUpdate, fk.OnDelete = pgAction(onUpdate), pgAction(onDelete)
		t.ForeignKeys = append(t.ForeignKeys, fk)
		return nil
	}, name)
	if err != nil {
		return t, err
	}
	err = queryRows(ctx, db, qPostgresIndexes, func(r *sql.Rows) error {
		var i Index
		var cols string
		if err := r.Scan(&i.Name, &i.Unique, &cols, &i.Where); err != nil {
			return err
		}
		i.Columns = strings.Split(cols, pgSep)
		t.Indexes = append(t.Indexes, i)
		return nil
	}, name)
	if err != nil {
		return t, err
	}
	err = queryRows(ctx, db, qPostgresChecks, func(r *sql.Rows) error {
		var check string
		if err := r.Scan(&check); err != nil {
			return err
		}
		t.Checks = append(t.Checks, check)
		return nil
	}, name)
	return t, err
}

// pgDefault is a column's Default and HasDefault. An identity column has no default expression, its sequence fills
// it; a generated column's expression sits where a default's would (pg_attrdef) and is no default.
func pgDefault(def sql.NullString, identity, generated string) (string, bool) {
	switch {
	case identity == "a":
		return "identity always", true
	case identity == "d":
		return "identity by default", true
	case generated != "":
		return "generated: " + def.String, true
	}
	return def.String, def.Valid
}

// pgAction spells pg_constraint's one-letter referential action as SQL does.
func pgAction(code string) string {
	switch code {
	case "a":
		return "NO ACTION"
	case "r":
		return "RESTRICT"
	case "c":
		return "CASCADE"
	case "n":
		return "SET NULL"
	case "d":
		return "SET DEFAULT"
	}
	return code
}
