package schemadoc

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

// sqliteFixture is what SQLite's catalog reports for a small schema: a parent, a child with a cascading FK and
// three kinds of index, a composite PK and a composite FK onto it, an FK naming no parent column, a vec0 and an
// fts5 table with their shadow tables, a plain table whose name starts like the fts5 table's, and the migration
// bookkeeping tables.
func sqliteFixture() *fakeDB {
	f := newFake()
	f.on(qSQLiteTables, nil,
		row("channels", "CREATE TABLE channels (id TEXT PRIMARY KEY, name TEXT NOT NULL, subscribers INTEGER NOT NULL DEFAULT 0, added_at TEXT)"),
		row("goose_db_version", "CREATE TABLE goose_db_version (id INTEGER PRIMARY KEY)"),
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
		row("id", "TEXT", int64(0), nil, int64(1)),
		row("name", "TEXT", int64(1), nil, int64(0)),
		row("subscribers", "INTEGER", int64(1), "0", int64(0)),
		row("added_at", "TEXT", int64(0), nil, int64(0)),
	)
	f.on(qSQLiteForeignKeys, args("channels"))
	f.on(qSQLiteIndexes, args("channels"), row("sqlite_autoindex_channels_1", int64(1), "pk", ""))

	f.on(qSQLiteColumns, args("search"), row("title", "", int64(0), nil, int64(0)), row("body", "", int64(0), nil, int64(0)))

	f.on(qSQLiteColumns, args("search_history"),
		row("query", "TEXT", int64(1), nil, int64(0)),
		row("at", "TEXT", int64(1), "CURRENT_TIMESTAMP", int64(0)),
	)
	f.on(qSQLiteForeignKeys, args("search_history"))
	f.on(qSQLiteIndexes, args("search_history"), row("sqlite_autoindex_search_history_1", int64(1), "u", ""))
	f.on(qSQLiteIndexColumns, args("sqlite_autoindex_search_history_1"), row(int64(0), "query", int64(0)))

	f.on(qSQLiteColumns, args("tag_notes"),
		row("video_id", "TEXT", int64(0), nil, int64(0)),
		row("tag", "TEXT", int64(0), nil, int64(0)),
		row("note", "TEXT", int64(0), nil, int64(0)),
	)
	f.on(qSQLiteForeignKeys, args("tag_notes"),
		row(int64(0), "tags", "video_id", nil, "NO ACTION", "NO ACTION"),
		row(int64(0), "tags", "tag", nil, "NO ACTION", "NO ACTION"),
	)
	f.on(qSQLiteIndexes, args("tag_notes"))

	f.on(qSQLiteColumns, args("tags"),
		row("video_id", "TEXT", int64(1), nil, int64(1)),
		row("tag", "TEXT", int64(1), nil, int64(2)),
	)
	f.on(qSQLiteForeignKeys, args("tags"), row(int64(0), "videos", "video_id", nil, "NO ACTION", "CASCADE"))
	f.on(qSQLiteIndexes, args("tags"), row("sqlite_autoindex_tags_1", int64(1), "pk", ""))

	f.on(qSQLiteColumns, args("vec_chunks"),
		row("chunk_id", "INTEGER", int64(0), nil, int64(1)),
		row("embedding", "float[4]", int64(0), nil, int64(0)),
	)

	f.on(qSQLiteColumns, args("videos"),
		row("id", "TEXT", int64(0), nil, int64(1)),
		row("channel_id", "TEXT", int64(1), nil, int64(0)),
		row("title", "TEXT", int64(1), nil, int64(0)),
		row("status", "TEXT", int64(1), "'new'", int64(0)),
		row("published_at", "TEXT", int64(0), nil, int64(0)),
		row("note", "", int64(0), "'a|b'", int64(0)),
	)
	f.on(qSQLiteForeignKeys, args("videos"), row(int64(0), "channels", "channel_id", "id", "NO ACTION", "CASCADE"))
	f.on(qSQLiteIndexes, args("videos"),
		row("idx_videos_channel", int64(0), "c", "CREATE INDEX idx_videos_channel ON videos(channel_id, published_at DESC)"),
		row("idx_videos_new", int64(0), "c", "CREATE INDEX idx_videos_new ON videos(status) WHERE status = 'new'"),
		row("idx_videos_title", int64(1), "c", "CREATE UNIQUE INDEX idx_videos_title ON videos(lower(title))"),
		row("sqlite_autoindex_videos_1", int64(1), "pk", ""),
	)
	f.on(qSQLiteIndexColumns, args("idx_videos_channel"), row(int64(1), "channel_id", int64(0)), row(int64(4), "published_at", int64(1)))
	f.on(qSQLiteIndexColumns, args("idx_videos_new"), row(int64(3), "status", int64(0)))
	f.on(qSQLiteIndexColumns, args("idx_videos_title"), row(int64(-2), nil, int64(0)))
	return f
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
	want := []string{"channels", "search", "search_history", "tag_notes", "tags", "vec_chunks", "videos"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("tables = %v, want %v", names, want)
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
	if s.table("search").Module != "fts5" || s.table("videos").Module != "" {
		t.Errorf("modules: search %q, videos %q", s.table("search").Module, s.table("videos").Module)
	}
}

func TestSQLiteColumns(t *testing.T) {
	cols := describeFixture(t).table("channels").Columns
	want := []Column{
		{Name: "id", Type: "TEXT", PK: 1},
		{Name: "name", Type: "TEXT", NotNull: true},
		{Name: "subscribers", Type: "INTEGER", NotNull: true, Default: "0", HasDefault: true},
		{Name: "added_at", Type: "TEXT"},
	}
	if !reflect.DeepEqual(cols, want) {
		t.Errorf("columns = %+v\nwant %+v", cols, want)
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
		{Name: "idx_videos_channel", Columns: []string{"channel_id", "published_at DESC"}},
		{Name: "idx_videos_new", Columns: []string{"status"}, Where: "status = 'new'"},
		{Name: "idx_videos_title", Columns: []string{"(expression)"}, Unique: true},
	}
	if got := s.table("videos").Indexes; !reflect.DeepEqual(got, want) {
		t.Errorf("videos indexes = %+v\nwant %+v", got, want)
	}
	// A UNIQUE constraint's autoindex is kept, a primary key's is not: the PK column says so already.
	if got := s.table("search_history").Indexes; len(got) != 1 || !got[0].Unique || got[0].Columns[0] != "query" {
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
