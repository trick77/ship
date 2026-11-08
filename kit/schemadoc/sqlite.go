package schemadoc

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// The catalog queries. Every table-valued pragma takes the table or index name as a bound argument.
const (
	qSQLiteTables = `SELECT name, sql FROM sqlite_master
		WHERE type = 'table' AND name NOT LIKE 'sqlite\_%' ESCAPE '\' ORDER BY name`
	qSQLiteColumns     = `SELECT name, type, "notnull", dflt_value, pk FROM pragma_table_info(?) ORDER BY cid`
	qSQLiteForeignKeys = `SELECT id, "table", "from", "to", on_update, on_delete
		FROM pragma_foreign_key_list(?) ORDER BY id, seq`
	qSQLiteIndexes = `SELECT il.name, il."unique", il.origin, COALESCE(m.sql, '')
		FROM pragma_index_list(?) AS il LEFT JOIN sqlite_master AS m ON m.type = 'index' AND m.name = il.name
		ORDER BY il.name`
	qSQLiteIndexColumns = `SELECT cid, name, "desc" FROM pragma_index_xinfo(?) WHERE key = 1 ORDER BY seqno`
)

var (
	// usingModule is a virtual table's module and its arguments in its CREATE statement.
	usingModule = regexp.MustCompile(`(?is)^\s*CREATE\s+VIRTUAL\s+TABLE\s+.*?\bUSING\s+(\w+)\s*(?:\((.*)\))?\s*;?\s*$`)
	// partialWhere is a partial index's predicate: what follows WHERE after the column list.
	partialWhere = regexp.MustCompile(`(?is)\)\s*WHERE\s+(.+?)\s*;?\s*$`)
	// shadowSuffix is, per module, the suffixes of the tables it keeps its data in, <table>_<suffix>. The module
	// names those tables itself; a user table never shares them.
	shadowSuffix = map[string]*regexp.Regexp{
		"vec0":  regexp.MustCompile(`^(chunks|rowids|info|auxiliary|vector_chunks\d+|metadatachunks\d+|metadatatext\d+)$`),
		"fts5":  regexp.MustCompile(`^(data|idx|content|docsize|config)$`),
		"fts4":  regexp.MustCompile(`^(content|segments|segdir|docsize|stat)$`),
		"fts3":  regexp.MustCompile(`^(content|segments|segdir|docsize|stat)$`),
		"rtree": regexp.MustCompile(`^(node|parent|rowid)$`),
	}
)

// isShadow reports whether name is a shadow table of one of the virtual tables (name to module).
func isShadow(name string, virtual map[string]string) bool {
	for vt, module := range virtual {
		suffix, ok := strings.CutPrefix(name, vt+"_")
		if re := shadowSuffix[module]; ok && re != nil && re.MatchString(suffix) {
			return true
		}
	}
	return false
}

func describeSQLite(ctx context.Context, db *sql.DB) ([]Table, error) {
	type entry struct{ name, sql string }
	var entries []entry
	err := queryRows(ctx, db, qSQLiteTables, func(r *sql.Rows) error {
		var e entry
		var create sql.NullString
		if err := r.Scan(&e.name, &create); err != nil {
			return err
		}
		e.sql = create.String
		entries = append(entries, e)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list tables: %w", err)
	}
	virtual, moduleArgs := map[string]string{}, map[string]string{}
	for _, e := range entries {
		if m := usingModule.FindStringSubmatch(e.sql); m != nil {
			virtual[e.name] = strings.ToLower(m[1])
			moduleArgs[e.name] = strings.Join(strings.Fields(m[2]), " ")
		}
	}
	var tables []Table
	for _, e := range entries {
		if slices.Contains(skipped, e.name) || isShadow(e.name, virtual) {
			continue
		}
		t, err := describeSQLiteTable(ctx, db, e.name, virtual[e.name])
		if err != nil {
			return nil, fmt.Errorf("describe %s: %w", e.name, err)
		}
		t.ModuleArgs = moduleArgs[e.name]
		tables = append(tables, t)
	}
	resolveImplicitRefs(tables)
	return tables, nil
}

func describeSQLiteTable(ctx context.Context, db *sql.DB, name, module string) (Table, error) {
	t := Table{Name: name, Module: module}
	err := queryRows(ctx, db, qSQLiteColumns, func(r *sql.Rows) error {
		var c Column
		var def sql.NullString
		if err := r.Scan(&c.Name, &c.Type, &c.NotNull, &def, &c.PK); err != nil {
			return err
		}
		c.Default, c.HasDefault = def.String, def.Valid
		t.Columns = append(t.Columns, c)
		return nil
	}, name)
	if err != nil || module != "" {
		// A virtual table has neither foreign keys nor indexes of its own.
		return t, err
	}
	if t.ForeignKeys, err = sqliteForeignKeys(ctx, db, name); err != nil {
		return t, err
	}
	t.Indexes, err = sqliteIndexes(ctx, db, name)
	return t, err
}

func sqliteForeignKeys(ctx context.Context, db *sql.DB, table string) ([]ForeignKey, error) {
	var fks []ForeignKey
	last := int64(-1)
	err := queryRows(ctx, db, qSQLiteForeignKeys, func(r *sql.Rows) error {
		var id int64
		var parent, from, onUpdate, onDelete string
		var to sql.NullString
		if err := r.Scan(&id, &parent, &from, &to, &onUpdate, &onDelete); err != nil {
			return err
		}
		if id != last {
			fks = append(fks, ForeignKey{RefTable: parent, OnUpdate: onUpdate, OnDelete: onDelete})
			last = id
		}
		fk := &fks[len(fks)-1]
		fk.Columns = append(fk.Columns, from)
		// NULL: the key names no parent column and means the parent's primary key, resolved once every
		// table is read.
		fk.RefColumns = append(fk.RefColumns, to.String)
		return nil
	}, table)
	return fks, err
}

// resolveImplicitRefs fills the parent columns an FK left unnamed with the parent's primary key.
func resolveImplicitRefs(tables []Table) {
	s := Schema{Tables: tables}
	for i := range tables {
		for j := range tables[i].ForeignKeys {
			fk := &tables[i].ForeignKeys[j]
			parent := s.table(fk.RefTable)
			if parent == nil {
				continue
			}
			pk := parent.pkColumns()
			for k, c := range fk.RefColumns {
				if c == "" && k < len(pk) {
					fk.RefColumns[k] = pk[k]
				}
			}
		}
	}
}

func sqliteIndexes(ctx context.Context, db *sql.DB, table string) ([]Index, error) {
	var idx []Index
	err := queryRows(ctx, db, qSQLiteIndexes, func(r *sql.Rows) error {
		var i Index
		var origin, create string
		if err := r.Scan(&i.Name, &i.Unique, &origin, &create); err != nil {
			return err
		}
		if origin == "pk" {
			return nil
		}
		if m := partialWhere.FindStringSubmatch(create); m != nil {
			i.Where = m[1]
		}
		idx = append(idx, i)
		return nil
	}, table)
	if err != nil {
		return nil, err
	}
	for k := range idx {
		if idx[k].Columns, err = sqliteIndexColumns(ctx, db, idx[k].Name); err != nil {
			return nil, err
		}
	}
	return idx, nil
}

func sqliteIndexColumns(ctx context.Context, db *sql.DB, index string) ([]string, error) {
	var cols []string
	err := queryRows(ctx, db, qSQLiteIndexColumns, func(r *sql.Rows) error {
		var cid int64
		var name sql.NullString
		var desc bool
		if err := r.Scan(&cid, &name, &desc); err != nil {
			return err
		}
		c := name.String
		if cid == -2 {
			c = "(expression)"
		}
		if desc {
			c += " DESC"
		}
		cols = append(cols, c)
		return nil
	}, index)
	if err != nil {
		return nil, fmt.Errorf("index %s: %w", index, err)
	}
	return cols, nil
}
