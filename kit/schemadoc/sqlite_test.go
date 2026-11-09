package schemadoc

import (
	"context"
	"database/sql/driver"
	"reflect"
	"strings"
	"testing"
)

// sqliteFixture is what SQLite's catalog reports for a small schema: a parent, a child with a cascading FK and
// four kinds of index, a composite PK and a composite FK onto it, an FK naming no parent column, rowid aliases (one
// keying a 1:1 child) and an INTEGER PRIMARY KEY DESC that is none, CHECK constraints, a vec0 and an fts5 table with
// their shadow tables, a plain table whose name starts like the fts5 table's, a view, and the migration bookkeeping
// tables.
func sqliteFixture() *fakeDB {
	f := newFake()
	f.on(qSQLiteTables, nil,
		row("channels", "CREATE TABLE channels (id TEXT PRIMARY KEY, name TEXT NOT NULL, subscribers INTEGER NOT NULL DEFAULT 0, added_at TEXT)"),
		row("goose_db_version", "CREATE TABLE goose_db_version (id INTEGER PRIMARY KEY)"),
		row("legacy_ids", "CREATE TABLE legacy_ids (id INTEGER PRIMARY KEY DESC, v TEXT)"),
		row("note_meta", "CREATE TABLE note_meta (note_id INTEGER PRIMARY KEY REFERENCES notes ON DELETE CASCADE, words INTEGER)"),
		row("notes", "CREATE TABLE notes (\n  id integer PRIMARY KEY,\n  video_id TEXT NOT NULL REFERENCES videos, -- check (no)\n"+
			"  body TEXT NOT NULL CHECK(body <> ')' AND\n    length(body) > 0),\n  \"check\" INTEGER,\n"+
			"  CONSTRAINT rank_range CHECK (\"check\" BETWEEN 1 -- lowest\n AND 5),\n"+
			"  body_len INTEGER GENERATED ALWAYS AS (length(body)) STORED\n)"),
		row("schema_migrations", "CREATE TABLE schema_migrations (name TEXT PRIMARY KEY)"),
		row("search", "CREATE VIRTUAL TABLE search USING fts5(title, body)"),
		row("search_config", "CREATE TABLE 'search_config'(k PRIMARY KEY, v) WITHOUT ROWID"),
		row("search_content", "CREATE TABLE 'search_content'(id INTEGER PRIMARY KEY, c0, c1)"),
		row("search_data", "CREATE TABLE 'search_data'(id INTEGER PRIMARY KEY, block BLOB)"),
		row("search_docsize", "CREATE TABLE 'search_docsize'(id INTEGER PRIMARY KEY, sz BLOB)"),
		row("search_history", "CREATE TABLE search_history (query TEXT NOT NULL UNIQUE, at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP)"),
		row("search_idx", "CREATE TABLE 'search_idx'(segid, term, pgno, PRIMARY KEY(segid, term)) WITHOUT ROWID"),
		row("tag_notes", "CREATE TABLE tag_notes (video_id TEXT, tag TEXT, note TEXT, FOREIGN KEY (video_id, tag) REFERENCES tags)"),
		row("tags", "CREATE TABLE tags (video_id TEXT NOT NULL REFERENCES videos ON DELETE CASCADE, tag TEXT NOT NULL, PRIMARY KEY (video_id, tag))"),
		row("vec_chunks", "create virtual table vec_chunks using vec0(\n    chunk_id integer primary key,\n    embedding float[4]\n)"),
		row("vec_chunks_chunks", "CREATE TABLE vec_chunks_chunks (chunk_id INTEGER PRIMARY KEY)"),
		row("vec_chunks_info", "CREATE TABLE vec_chunks_info (key TEXT PRIMARY KEY, value ANY)"),
		row("vec_chunks_rowids", "CREATE TABLE vec_chunks_rowids (rowid INTEGER PRIMARY KEY)"),
		row("vec_chunks_vector_chunks00", "CREATE TABLE vec_chunks_vector_chunks00 (rowid PRIMARY KEY, vectors BLOB)"),
		row("videos", "CREATE TABLE videos (...)"),
	)

	f.on(qSQLiteColumns, args("channels"),
		col("id", "TEXT", 0, nil, 1),
		col("name", "TEXT", 1, nil, 0),
		col("subscribers", "INTEGER", 1, "0", 0),
		col("added_at", "TEXT", 0, nil, 0),
	)
	f.on(qSQLiteForeignKeys, args("channels"))
	f.on(qSQLiteIndexes, args("channels"), row("sqlite_autoindex_channels_1", int64(1), "pk", ""))

	f.on(qSQLiteColumns, args("legacy_ids"), col("id", "INTEGER", 0, nil, 1), col("v", "TEXT", 0, nil, 0))
	f.on(qSQLiteForeignKeys, args("legacy_ids"))
	f.on(qSQLiteIndexes, args("legacy_ids"), row("sqlite_autoindex_legacy_ids_1", int64(1), "pk", ""))

	f.on(qSQLiteColumns, args("note_meta"), col("note_id", "INTEGER", 0, nil, 1), col("words", "INTEGER", 0, nil, 0))
	f.on(qSQLiteForeignKeys, args("note_meta"), row(int64(0), "notes", "note_id", nil, "NO ACTION", "CASCADE"))
	f.on(qSQLiteIndexes, args("note_meta"))

	f.on(qSQLiteColumns, args("notes"),
		col("id", "integer", 0, nil, 1),
		col("video_id", "TEXT", 1, nil, 0),
		col("body", "TEXT", 1, nil, 0),
		col("check", "INTEGER", 0, nil, 0),
		append(col("body_len", "INTEGER", 0, nil, 0)[:5], int64(3)), // stored generated
	)
	f.on(qSQLiteForeignKeys, args("notes"), row(int64(0), "videos", "video_id", nil, "NO ACTION", "NO ACTION"))
	f.on(qSQLiteIndexes, args("notes"))

	f.on(qSQLiteColumns, args("search"),
		col("title", "", 0, nil, 0), col("body", "", 0, nil, 0),
		append(col("search", "", 0, nil, 0)[:5], int64(1)), // fts5's hidden columns
		append(col("rank", "", 0, nil, 0)[:5], int64(1)),
	)

	f.on(qSQLiteColumns, args("search_history"),
		col("query", "TEXT", 1, nil, 0),
		col("at", "TEXT", 1, "CURRENT_TIMESTAMP", 0),
	)
	f.on(qSQLiteForeignKeys, args("search_history"))
	f.on(qSQLiteIndexes, args("search_history"), row("sqlite_autoindex_search_history_1", int64(1), "u", ""))
	f.on(qSQLiteIndexColumns, args("sqlite_autoindex_search_history_1"), key(0, "query", 0, "NOCASE"))

	f.on(qSQLiteColumns, args("tag_notes"),
		col("video_id", "TEXT", 0, nil, 0),
		col("tag", "TEXT", 0, nil, 0),
		col("note", "TEXT", 0, nil, 0),
	)
	f.on(qSQLiteForeignKeys, args("tag_notes"),
		row(int64(0), "tags", "video_id", nil, "NO ACTION", "NO ACTION"),
		row(int64(0), "tags", "tag", nil, "NO ACTION", "NO ACTION"),
	)
	f.on(qSQLiteIndexes, args("tag_notes"))

	f.on(qSQLiteColumns, args("tags"), col("video_id", "TEXT", 1, nil, 1), col("tag", "TEXT", 1, nil, 2))
	f.on(qSQLiteForeignKeys, args("tags"), row(int64(0), "videos", "video_id", nil, "NO ACTION", "CASCADE"))
	f.on(qSQLiteIndexes, args("tags"), row("sqlite_autoindex_tags_1", int64(1), "pk", ""))

	f.on(qSQLiteColumns, args("vec_chunks"),
		col("chunk_id", "INTEGER", 0, nil, 1),
		col("embedding", "float[4]", 0, nil, 0),
		col("rowid", "", 0, nil, 0),
	)

	f.on(qSQLiteColumns, args("videos"),
		col("id", "TEXT", 0, nil, 1),
		col("channel_id", "TEXT", 1, nil, 0),
		col("title", "TEXT", 1, nil, 0),
		col("status", "TEXT", 1, "'new'", 0),
		col("published_at", "TEXT", 0, nil, 0),
		col("note", "", 0, "'a|b'", 0),
	)
	f.on(qSQLiteForeignKeys, args("videos"), row(int64(0), "channels", "channel_id", "id", "NO ACTION", "CASCADE"))
	f.on(qSQLiteIndexes, args("videos"),
		row("idx_videos_channel", int64(0), "c", "CREATE INDEX idx_videos_channel ON videos(channel_id, published_at DESC)"),
		row("idx_videos_new", int64(0), "c", "CREATE INDEX idx_videos_new ON videos(status) WHERE status = 'new'"),
		row("idx_videos_recent", int64(0), "c", "CREATE INDEX idx_videos_recent ON \"videos\" (channel_id,\n"+
			"  coalesce(published_at, ')') COLLATE NOCASE DESC -- newest first\n) WHERE title <> ''"),
		row("idx_videos_title", int64(1), "c", "CREATE UNIQUE INDEX idx_videos_title ON videos(lower(title))"),
		row("sqlite_autoindex_videos_1", int64(1), "pk", ""),
	)
	f.on(qSQLiteIndexColumns, args("idx_videos_channel"), key(1, "channel_id", 0, "BINARY"), key(4, "published_at", 1, "BINARY"))
	f.on(qSQLiteIndexColumns, args("idx_videos_new"), key(3, "status", 0, "BINARY"))
	f.on(qSQLiteIndexColumns, args("idx_videos_recent"), key(1, "channel_id", 0, "BINARY"), key(-2, nil, 1, "NOCASE"))
	f.on(qSQLiteIndexColumns, args("idx_videos_title"), key(-2, nil, 0, "BINARY"))

	f.on(qSQLiteViews, nil, row("recent_videos"))
	return f
}

// col is one pragma_table_xinfo row of a column not hidden.
func col(name, typ string, notNull int64, dflt any, pk int64) []driver.Value {
	return row(name, typ, notNull, dflt, pk, int64(0))
}

// key is one pragma_index_xinfo row.
func key(cid int64, name any, desc int64, coll string) []driver.Value {
	return row(cid, name, desc, coll)
}

func describeFixture(t *testing.T) Schema {
	t.Helper()
	s, err := Describe(context.Background(), sqliteFixture().open(t), SQLite)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSQLiteTablesSkipBookkeepingAndShadows(t *testing.T) {
	s := describeFixture(t)
	var names []string
	for _, tb := range s.Tables {
		names = append(names, tb.Name)
	}
	want := []string{"channels", "legacy_ids", "note_meta", "notes", "search", "search_history", "tag_notes", "tags", "vec_chunks", "videos"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("tables = %v, want %v", names, want)
	}
	if want := []View{{Name: "recent_videos", Kind: "view"}}; !reflect.DeepEqual(s.Views, want) {
		t.Errorf("views = %+v, want %+v", s.Views, want)
	}
}

// A rowid alias is NOT NULL, though the pragma says otherwise; INTEGER PRIMARY KEY DESC is no alias (it has an
// index of its own) and, like a TEXT key, takes NULL.
func TestSQLiteRowidAlias(t *testing.T) {
	s := describeFixture(t)
	for _, c := range []struct {
		table string
		want  bool
	}{{"notes", true}, {"note_meta", true}, {"vec_chunks", true}, {"legacy_ids", false}, {"channels", false}} {
		if got := s.table(c.table).Columns[0].NotNull; got != c.want {
			t.Errorf("%s first column NotNull = %v, want %v", c.table, got, c.want)
		}
	}
	// Only a lone key column is the rowid.
	cols := []Column{{Name: "a", Type: "INTEGER", PK: 1}, {Name: "b", Type: "INTEGER", PK: 2}}
	markRowidAlias(cols, false)
	if cols[0].NotNull || cols[1].NotNull {
		t.Errorf("composite key marked NOT NULL: %+v", cols)
	}
}

func TestSQLiteChecks(t *testing.T) {
	s := describeFixture(t)
	want := []string{`CHECK (body <> ')' AND length(body) > 0)`, `CONSTRAINT rank_range CHECK ("check" BETWEEN 1 AND 5)`}
	if got := s.table("notes").Checks; !reflect.DeepEqual(got, want) {
		t.Errorf("notes checks = %q, want %q", got, want)
	}
	if got := s.table("videos").Checks; got != nil {
		t.Errorf("videos checks = %q, want none", got)
	}
}

func TestSQLiteChecksText(t *testing.T) {
	for create, want := range map[string][]string{
		"CREATE TABLE t (x)": nil,
		"CREATE TABLE t":     nil,
		"CREATE TABLE t (x CHECK (x > (1)), [check (y)] INT)":    {"CHECK (x > (1))"},
		"CREATE TABLE t (x /* CHECK (no) */, checked INT)":       nil,
		"CREATE TABLE t (x, CHECK (x <> 'it''s )'), check(x<9))": {"CHECK (x <> 'it''s )')", "CHECK (x<9)"},
		"CREATE TABLE t (x CHECK x)":                             nil,
		"CREATE TABLE t (x CHECK (x > 0)":                        nil,
		"CREATE TABLE t (x CHECK (x > 0 -- one\n AND x < 9))":    {"CHECK (x > 0 AND x < 9)"},
		"CREATE TABLE t (x DEFAULT (1) CHECK (x > 0))":           {"CHECK (x > 0)"},
		"CREATE TABLE t (x) WITHOUT ROWID, CHECK (y)":            nil,
		"CREATE TABLE t (x `a``b` CHECK (\"q\"\"(\" > 0))":       {"CHECK (\"q\"\"(\" > 0)"},
	} {
		if got := sqliteChecks(create); !reflect.DeepEqual(got, want) {
			t.Errorf("sqliteChecks(%q) = %q, want %q", create, got, want)
		}
	}
}

func TestIndexKey(t *testing.T) {
	for _, c := range []struct {
		create string
		n      int
		want   string
	}{
		{"CREATE INDEX i ON t (a, lower(b) DESC)", 1, "lower(b)"},
		{"CREATE INDEX \"i(\" ON t(a /* , */, b + 1 asc)", 1, "b + 1"},
		{"CREATE INDEX i ON t(a, b)", 2, "(expression)"},
		{"CREATE INDEX i ON t", 0, "(expression)"},
		{"", 0, "(expression)"},
		{"CREATE INDEX i ON t(a, lower(b)", 1, "(expression)"},
		{"CREATE INDEX i ON t(a, )", 1, "(expression)"},
		{"CREATE INDEX i ON t(a -- x\n, b)", 1, "b"},
		{"CREATE INDEX i ON t([a b] || 'x', b)", 0, "[a b] || 'x'"},
		{"CREATE INDEX i ON t(lower(b) DESC -- newest\n)", 0, "lower(b)"},
		{"CREATE INDEX i ON t(lower(b) COLLATE \"my coll\" DESC)", 0, "lower(b)"},
		{"CREATE INDEX i ON t(lower(b) COLLATE nocase)", 0, "lower(b)"},
		// Only a top-level tail is the key's: inside parentheses or a literal it is the expression.
		{"CREATE INDEX i ON t(lower(a COLLATE nocase))", 0, "lower(a COLLATE nocase)"},
		{"CREATE INDEX i ON t(a || ' COLLATE x')", 0, "a || ' COLLATE x'"},
		{"CREATE INDEX i ON t(a || ' DESC' ASC)", 0, "a || ' DESC'"},
		{"CREATE INDEX i ON t(adesc + 1)", 0, "adesc + 1"},
		{"CREATE INDEX i ON t(a COLLATE)", 0, "a COLLATE"},
		{"CREATE INDEX i ON t(a COLLATE x y)", 0, "a COLLATE x y"},
		{"CREATE INDEX i ON t(DESC)", 0, "(expression)"},
	} {
		if got := indexKey(c.create, c.n); got != c.want {
			t.Errorf("indexKey(%q, %d) = %q, want %q", c.create, c.n, got, c.want)
		}
	}
}

func TestSQLiteVirtualTables(t *testing.T) {
	s := describeFixture(t)
	vec := s.table("vec_chunks")
	if vec.Module != "vec0" {
		t.Errorf("vec_chunks module = %q, want vec0", vec.Module)
	}
	// vec0 declares its columns untyped to SQLite: the vector width is only in the module arguments.
	if want := "chunk_id integer primary key, embedding float[4]"; vec.ModuleArgs != want {
		t.Errorf("vec_chunks args = %q, want %q", vec.ModuleArgs, want)
	}
	if got := vec.Columns[1]; got.Name != "embedding" || got.Type != "float[4]" {
		t.Errorf("vec_chunks column = %+v", got)
	}
	// chunk_id is the declared key: the declared rowid duplicates it (moduleRowid). fts5's hidden columns are none
	// of a row.
	if len(vec.Columns) != 2 {
		t.Errorf("vec_chunks columns = %+v, want the rowid dropped beside chunk_id", vec.Columns)
	}
	if got := s.table("search").Columns; len(got) != 2 {
		t.Errorf("search columns = %+v, want the hidden ones dropped", got)
	}
	if s.table("search").Module != "fts5" || s.table("videos").Module != "" {
		t.Errorf("modules: search %q, videos %q", s.table("search").Module, s.table("videos").Module)
	}
}

// A CHECK's name is the CONSTRAINT <name> right before it, quoted or not; one naming another constraint is not its.
func TestCheckLists(t *testing.T) {
	for def, want := range map[string][]string{
		`x INT CHECK (x > 0)`:                        {`CHECK (x > 0)`},
		`x INT CONSTRAINT pos CHECK (x > 0)`:         {`CONSTRAINT pos CHECK (x > 0)`},
		`CONSTRAINT "a b"  CHECK(x)`:                 {`CONSTRAINT "a b" CHECK (x)`},
		"CONSTRAINT `t` CHECK (x)":                   {"CONSTRAINT `t` CHECK (x)"},
		`CONSTRAINT [t] CHECK (x)`:                   {`CONSTRAINT [t] CHECK (x)`},
		`CONSTRAINT "a""b" CHECK (x)`:                {`CONSTRAINT "a""b" CHECK (x)`},
		`CONSTRAINT "a""" CHECK (x)`:                 {`CONSTRAINT "a""" CHECK (x)`},
		`x INT CONSTRAINT nn NOT NULL CHECK (x > 0)`: {`CHECK (x > 0)`},
		`x TEXT DEFAULT 'CONSTRAINT y' CHECK (x)`:    {`CHECK (x)`},
		`x INT CHECK (x > 0) CONSTRAINT y CHECK (x)`: {`CHECK (x > 0)`, `CONSTRAINT y CHECK (x)`},
		`"unclosed CHECK (x)`:                        nil,
	} {
		if got := checkLists(def); !reflect.DeepEqual(got, want) {
			t.Errorf("checkLists(%q) = %q, want %q", def, got, want)
		}
	}
}

// A module's declared rowid without a key column beside it is the key (vec0 keyed on chunks.id); beside one, a
// duplicate.
func TestModuleRowid(t *testing.T) {
	got := moduleRowid([]Column{{Name: "embedding", Type: "float[4]"}, {Name: "rowid"}})
	want := []Column{{Name: "embedding", Type: "float[4]"}, {Name: "rowid", Type: "INTEGER", NotNull: true, PK: 1}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("keyless: %+v, want %+v", got, want)
	}
	got = moduleRowid([]Column{{Name: "id", Type: "INTEGER", PK: 1}, {Name: "rowid"}})
	if len(got) != 1 || got[0].Name != "id" {
		t.Errorf("keyed: %+v, want the rowid dropped", got)
	}
	// A rowid declared the primary key itself is that key.
	got = moduleRowid([]Column{{Name: "rowid", PK: 1}, {Name: "v"}})
	if len(got) != 2 || got[0].PK != 1 || !got[0].NotNull {
		t.Errorf("rowid PK: %+v, want it kept as the key", got)
	}
	if got := moduleRowid([]Column{{Name: "a"}}); len(got) != 1 {
		t.Errorf("no rowid: %+v", got)
	}
}

func TestSQLiteColumns(t *testing.T) {
	cols := describeFixture(t).table("channels").Columns
	// A TEXT primary key takes NULL in a rowid table: SQLite's old quirk, kept for compatibility.
	want := []Column{
		{Name: "id", Type: "TEXT", PK: 1},
		{Name: "name", Type: "TEXT", NotNull: true},
		{Name: "subscribers", Type: "INTEGER", NotNull: true, Default: "0", HasDefault: true},
		{Name: "added_at", Type: "TEXT"},
	}
	if !reflect.DeepEqual(cols, want) {
		t.Errorf("columns = %+v\nwant %+v", cols, want)
	}
	// table_xinfo lists a generated column, its expression read from the CREATE statement.
	gen := describeFixture(t).table("notes").Columns[4]
	if want := (Column{Name: "body_len", Type: "INTEGER", Default: "generated: length(body)", HasDefault: true}); gen != want {
		t.Errorf("generated column = %+v, want %+v", gen, want)
	}
}

func TestSQLiteGenerated(t *testing.T) {
	for _, c := range []struct {
		create, col, want string
	}{
		{"CREATE TABLE t (a, b NUMERIC(10, 2) AS (a * 2))", "b", "a * 2"},
		{"CREATE TABLE t (a, \"B c\" INT CHECK (CAST(a AS INT) > 0) GENERATED ALWAYS AS (a || 'x') VIRTUAL)", "b C", "a || 'x'"},
		{"CREATE TABLE t (a, [b] AS (a))", "b", "a"},
		{"CREATE TABLE t (a, `b` AS (a))", "b", "a"},
		{"CREATE TABLE t (a, 'b''x' AS (a))", "b'x", "a"},
		{"CREATE TABLE t (a, b AS a)", "b", ""},
		{"CREATE TABLE t (a, b AS (a)", "b", ""},
		{"CREATE TABLE t (a, b INT(", "b", ""},
		{"CREATE TABLE t (a, b)", "b", ""},
		{"CREATE TABLE t (a, b)", "c", ""},
	} {
		if got := sqliteGenerated(c.create, c.col); got != c.want {
			t.Errorf("sqliteGenerated(%q, %q) = %q, want %q", c.create, c.col, got, c.want)
		}
	}
	// No expression found: still marked generated.
	f := sqliteFixture()
	f.on(qSQLiteColumns, args("tags"), col("video_id", "TEXT", 1, nil, 1), append(col("tag", "TEXT", 1, nil, 2)[:5], int64(2)))
	s, err := Describe(context.Background(), f.open(t), SQLite)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.table("tags").Columns[1]; got.Default != "generated" || !got.HasDefault {
		t.Errorf("generated column without expression = %+v", got)
	}
}

func TestLeadingName(t *testing.T) {
	for def, want := range map[string]string{
		"a INT": "a", "a": "a", `"a ""b""" INT`: `a "b"`, "[a b] INT": "a b", "`a` INT": "a", "": "",
		`"abc`: "abc", "[abc": "abc", `"`: "",
	} {
		if got := leadingName(def); got != want {
			t.Errorf("leadingName(%q) = %q, want %q", def, got, want)
		}
	}
}

func TestIndexWhere(t *testing.T) {
	for create, want := range map[string]string{
		"CREATE INDEX i ON t(a)":                                    "",
		"CREATE INDEX i ON t(a) WHERE a > 0;":                       "a > 0",
		"CREATE INDEX i ON t(coalesce(a, ') where z')) WHERE b>0":   "b>0",
		"CREATE INDEX i ON t(a) -- partial\n WHERE\n a IS NOT NULL": "a IS NOT NULL",
		"CREATE INDEX i ON t(a":                                     "",
		"CREATE INDEX i ON t(a) WHEREVER":                           "",
	} {
		if got := indexWhere(create); got != want {
			t.Errorf("indexWhere(%q) = %q, want %q", create, got, want)
		}
	}
}

func TestStripComments(t *testing.T) {
	in := "a -- x\nb /* y */ c '--' \"/*\" /* open"
	if got, want := stripComments(in), "a  b   c '--' \"/*\"  "; got != want {
		t.Errorf("stripComments = %q, want %q", got, want)
	}
}

func TestSQLiteForeignKeys(t *testing.T) {
	s := describeFixture(t)
	for _, c := range []struct {
		table string
		want  []ForeignKey
	}{
		{"videos", []ForeignKey{{Columns: []string{"channel_id"}, RefTable: "channels", RefColumns: []string{"id"}, OnUpdate: "NO ACTION", OnDelete: "CASCADE"}}},
		// No parent column named: the parent's primary key, in its order.
		{"tags", []ForeignKey{{Columns: []string{"video_id"}, RefTable: "videos", RefColumns: []string{"id"}, OnUpdate: "NO ACTION", OnDelete: "CASCADE"}}},
		{"tag_notes", []ForeignKey{{Columns: []string{"video_id", "tag"}, RefTable: "tags", RefColumns: []string{"video_id", "tag"}, OnUpdate: "NO ACTION", OnDelete: "NO ACTION"}}},
	} {
		if got := s.table(c.table).ForeignKeys; !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s FKs = %+v\nwant %+v", c.table, got, c.want)
		}
	}
}

func TestSQLiteIndexes(t *testing.T) {
	s := describeFixture(t)
	want := []Index{
		// Keys are the bare column names, "" for an expression.
		{Name: "idx_videos_channel", Columns: []string{"channel_id", "published_at DESC"},
			Keys: []string{"channel_id", "published_at"}},
		{Name: "idx_videos_new", Columns: []string{"status"}, Keys: []string{"status"}, Where: "status = 'new'"},
		// An expression is its text; the pragma's COLLATE and DESC are appended once, the written ones (and the
		// comment) dropped.
		{Name: "idx_videos_recent", Columns: []string{"channel_id", "coalesce(published_at, ')') COLLATE NOCASE DESC"},
			Keys: []string{"channel_id", ""}, Where: "title <> ''"},
		{Name: "idx_videos_title", Columns: []string{"lower(title)"}, Keys: []string{""}, Unique: true},
	}
	if got := s.table("videos").Indexes; !reflect.DeepEqual(got, want) {
		t.Errorf("videos indexes = %+v\nwant %+v", got, want)
	}
	// A UNIQUE constraint's autoindex is kept, a primary key's is not: the PK column says so already. Its key's
	// collation comes from the column.
	if got := s.table("search_history").Indexes; len(got) != 1 || !got[0].Unique || got[0].Columns[0] != "query COLLATE NOCASE" {
		t.Errorf("search_history indexes = %+v", got)
	}
	if got := s.table("tags").Indexes; len(got) != 0 {
		t.Errorf("tags indexes = %+v, want none", got)
	}
}

func TestSQLiteQueryErrors(t *testing.T) {
	for _, c := range []struct {
		q   string
		arg []any
	}{
		{qSQLiteTables, nil},
		{qSQLiteColumns, args("videos")},
		{qSQLiteColumns, args("vec_chunks")},
		{qSQLiteForeignKeys, args("videos")},
		{qSQLiteIndexes, args("videos")},
		{qSQLiteIndexColumns, args("idx_videos_new")},
		{qSQLiteViews, nil},
	} {
		f := sqliteFixture()
		f.failOn(c.q, c.arg...)
		if _, err := Describe(context.Background(), f.open(t), SQLite); err == nil || !strings.Contains(err.Error(), "boom") {
			t.Errorf("%.40s %v: err = %v, want boom", c.q, c.arg, err)
		}
	}
}

func TestShadowNames(t *testing.T) {
	virtual := map[string]string{"v": "vec0", "f": "fts5", "x": "rtree"}
	for name, want := range map[string]bool{
		"v_chunks": true, "v_rowids": true, "v_info": true, "v_auxiliary": true,
		"v_vector_chunks00": true, "v_vector_chunks12": true, "v_metadatachunks01": true, "v_metadatatext03": true,
		"f_data": true, "f_idx": true, "f_content": true, "f_docsize": true, "f_config": true,
		"x_node": true, "x_parent": true, "x_rowid": true,
		"v_data": false, "f_chunks": false, "v_vector_chunks": false, "v": false, "w_chunks": false, "f_history": false,
	} {
		if got := isShadow(name, virtual); got != want {
			t.Errorf("isShadow(%q) = %v, want %v", name, got, want)
		}
	}
}
