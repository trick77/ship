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
	// table_xinfo, not table_info: only it lists generated columns (hidden 2 virtual, 3 stored). Hidden 1 is a
	// virtual table's hidden column (fts5's rank), no column of its rows.
	qSQLiteColumns     = `SELECT name, type, "notnull", dflt_value, pk, hidden FROM pragma_table_xinfo(?) ORDER BY cid`
	qSQLiteForeignKeys = `SELECT id, "table", "from", "to", on_update, on_delete
		FROM pragma_foreign_key_list(?) ORDER BY id, seq`
	qSQLiteIndexes = `SELECT il.name, il."unique", il.origin, COALESCE(m.sql, '')
		FROM pragma_index_list(?) AS il LEFT JOIN sqlite_master AS m ON m.type = 'index' AND m.name = il.name
		ORDER BY il.name`
	qSQLiteIndexColumns = `SELECT cid, name, "desc", coll FROM pragma_index_xinfo(?) WHERE key = 1 ORDER BY seqno`
	qSQLiteViews        = `SELECT name FROM sqlite_master WHERE type = 'view' ORDER BY name`
)

var (
	// usingModule is a virtual table's module and its arguments in its CREATE statement.
	usingModule = regexp.MustCompile(`(?is)^\s*CREATE\s+VIRTUAL\s+TABLE\s+.*?\bUSING\s+(\w+)\s*(?:\((.*)\))?\s*;?\s*$`)
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

func describeSQLite(ctx context.Context, db *sql.DB) (Schema, error) {
	tables, err := sqliteTables(ctx, db)
	if err != nil {
		return Schema{}, err
	}
	var views []View
	err = queryRows(ctx, db, qSQLiteViews, func(r *sql.Rows) error {
		v := View{Kind: "view"}
		if err := r.Scan(&v.Name); err != nil {
			return err
		}
		views = append(views, v)
		return nil
	})
	if err != nil {
		return Schema{}, fmt.Errorf("list views: %w", err)
	}
	return Schema{Tables: tables, Views: views}, nil
}

func sqliteTables(ctx context.Context, db *sql.DB) ([]Table, error) {
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
			moduleArgs[e.name] = oneLine(m[2])
		}
	}
	var tables []Table
	for _, e := range entries {
		if slices.Contains(skipped, e.name) || isShadow(e.name, virtual) {
			continue
		}
		t, err := describeSQLiteTable(ctx, db, e.name, e.sql, virtual[e.name])
		if err != nil {
			return nil, fmt.Errorf("describe %s: %w", e.name, err)
		}
		t.ModuleArgs = moduleArgs[e.name]
		tables = append(tables, t)
	}
	resolveImplicitRefs(tables)
	return tables, nil
}

func describeSQLiteTable(ctx context.Context, db *sql.DB, name, create, module string) (Table, error) {
	t := Table{Name: name, Module: module}
	err := queryRows(ctx, db, qSQLiteColumns, func(r *sql.Rows) error {
		var c Column
		var def sql.NullString
		var hidden int64
		if err := r.Scan(&c.Name, &c.Type, &c.NotNull, &def, &c.PK, &hidden); err != nil {
			return err
		}
		if hidden == 1 {
			return nil
		}
		c.Default, c.HasDefault = def.String, def.Valid
		if hidden == 2 || hidden == 3 {
			c.Default, c.HasDefault = strings.TrimSuffix("generated: "+sqliteGenerated(create, c.Name), ": "), true
		}
		t.Columns = append(t.Columns, c)
		return nil
	}, name)
	if err != nil {
		return t, err
	}
	if module != "" {
		// A virtual table has neither foreign keys, indexes nor constraints of its own.
		markRowidAlias(t.Columns, false)
		t.Columns = moduleRowid(t.Columns, module)
		return t, nil
	}
	if t.ForeignKeys, err = sqliteForeignKeys(ctx, db, name); err != nil {
		return t, err
	}
	var pkIndex bool
	if t.Indexes, pkIndex, err = sqliteIndexes(ctx, db, name); err != nil {
		return t, err
	}
	markRowidAlias(t.Columns, pkIndex)
	t.Checks = sqliteChecks(create)
	return t, nil
}

// ftsModules are the full-text modules: they declare no key column, yet their integer rowid (fts3/fts4's docid) is
// the key they have and what code joins on (rowid = chunks.id, content_rowid=).
var ftsModules = []string{"fts3", "fts4", "fts5"}

// moduleRowid is a virtual table's columns with its rowid as the key it is, the way a reader learns the rows key on
// another table's id (rowid = chunks.id): the rowid a module declares (vec0 does) kept and marked the NOT NULL
// INTEGER primary key, a full-text table's implicit one put first. A table declaring a primary key column of its own
// keys on that one: a declared rowid is dropped, a duplicate. A plain table's implicit rowid stays unshown: such a
// table has a key of its own.
func moduleRowid(cols []Column, module string) []Column {
	rowid := Column{Name: "rowid", Type: "INTEGER", NotNull: true, PK: 1}
	i := slices.IndexFunc(cols, func(c Column) bool { return strings.EqualFold(c.Name, "rowid") })
	if i < 0 {
		if slices.Contains(ftsModules, module) {
			return append([]Column{rowid}, cols...)
		}
		return cols
	}
	for j, c := range cols {
		if j != i && c.PK > 0 {
			return slices.Delete(cols, i, i+1)
		}
	}
	rowid.Name = cols[i].Name
	cols[i] = rowid
	return cols
}

// markRowidAlias marks a rowid alias NOT NULL: the pragma reports it nullable, yet it is the rowid itself. It is a
// lone primary key column declared exactly INTEGER that has no index of its own (INTEGER PRIMARY KEY DESC has one and
// is no alias). Any other primary key column of a rowid table really takes NULL; a WITHOUT ROWID table's are reported
// NOT NULL already.
func markRowidAlias(cols []Column, pkIndex bool) {
	var pk []int
	for i, c := range cols {
		if c.PK > 0 {
			pk = append(pk, i)
		}
	}
	if len(pk) == 1 && !pkIndex && strings.EqualFold(cols[pk[0]].Type, "INTEGER") {
		cols[pk[0]].NotNull = true
	}
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

// sqliteIndexes is table's indexes but its primary key's, and whether that one exists.
func sqliteIndexes(ctx context.Context, db *sql.DB, table string) ([]Index, bool, error) {
	var idx []Index
	var creates []string
	pkIndex := false
	err := queryRows(ctx, db, qSQLiteIndexes, func(r *sql.Rows) error {
		var i Index
		var origin, create string
		if err := r.Scan(&i.Name, &i.Unique, &origin, &create); err != nil {
			return err
		}
		if origin == "pk" {
			pkIndex = true
			return nil
		}
		i.Where = indexWhere(create)
		idx = append(idx, i)
		creates = append(creates, create)
		return nil
	}, table)
	if err != nil {
		return nil, false, err
	}
	for k := range idx {
		if idx[k].Columns, idx[k].Keys, err = sqliteIndexColumns(ctx, db, idx[k].Name, creates[k]); err != nil {
			return nil, false, err
		}
	}
	return idx, pkIndex, nil
}

// sqliteIndexColumns is an index's key columns, with " COLLATE <name>" where not BINARY and " DESC" where
// descending; an expression (cid -2) is its text in create. keys is the same keys' bare column names, "" for an
// expression.
func sqliteIndexColumns(ctx context.Context, db *sql.DB, index, create string) (cols, keys []string, err error) {
	err = queryRows(ctx, db, qSQLiteIndexColumns, func(r *sql.Rows) error {
		var cid int64
		var name, coll sql.NullString
		var desc bool
		if err := r.Scan(&cid, &name, &desc, &coll); err != nil {
			return err
		}
		c := name.String
		if cid == -2 {
			c = indexKey(create, len(cols))
		}
		keys = append(keys, name.String)
		// The collation the key compares with, written on it or inherited from its column; BINARY is the default.
		if coll.Valid && !strings.EqualFold(coll.String, "BINARY") {
			c += " COLLATE " + coll.String
		}
		if desc {
			c += " DESC"
		}
		cols = append(cols, c)
		return nil
	}, index)
	if err != nil {
		return nil, nil, fmt.Errorf("index %s: %w", index, err)
	}
	return cols, keys, nil
}
