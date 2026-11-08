package schemadoc

import (
	"context"
	"database/sql"
	"reflect"
	"strings"
	"testing"
)

// postgresFixture is what pg_catalog reports for a parent and a child table, plus the bookkeeping table: identity
// and generated columns, a CHECK constraint, a view and a materialized view.
func postgresFixture() *fakeDB {
	f := newFake()
	f.on(qPostgresTables, nil, row("channels"), row("schema_migrations"), row("videos"))
	f.on(qPostgresViews, nil, row("channel_stats", "m"), row("active_videos", "v"))

	f.on(qPostgresColumns, args("channels"),
		row("id", "text", true, nil, "", "", int64(1)),
		row("subscribers", "bigint", true, "0", "", "", int64(0)),
		row("seq", "bigint", true, nil, "a", "", int64(0)),
	)
	f.on(qPostgresForeignKeys, args("channels"))
	f.on(qPostgresIndexes, args("channels"))
	f.on(qPostgresChecks, args("channels"), row("CHECK (subscribers >= 0)"))

	f.on(qPostgresColumns, args("videos"),
		row("id", "bigint", true, nil, "d", "", int64(1)),
		row("channel_id", "text", false, nil, "", "", int64(0)),
		row("embedding", "vector(768)", false, nil, "", "", int64(0)),
		row("added_at", "timestamp with time zone", true, "now()", "", "", int64(0)),
		row("title_lc", "text", false, "lower(title)", "", "s", int64(0)),
	)
	f.on(qPostgresForeignKeys, args("videos"), row("videos_channel_id_fkey", "channels", "channel_id", "id", "a", "n"))
	f.on(qPostgresIndexes, args("videos"),
		row("idx_videos_channel", false, "channel_id\x1fadded_at", ""),
		row("idx_videos_lower", true, "lower(id)", "channel_id IS NOT NULL"),
	)
	f.on(qPostgresChecks, args("videos"))
	return f
}

func TestPostgresDescribe(t *testing.T) {
	s, err := Describe(context.Background(), postgresFixture().open(t), Postgres)
	if err != nil {
		t.Fatal(err)
	}
	want := Schema{Tables: []Table{
		{Name: "channels", Columns: []Column{
			{Name: "id", Type: "text", NotNull: true, PK: 1},
			{Name: "subscribers", Type: "bigint", NotNull: true, Default: "0", HasDefault: true},
			{Name: "seq", Type: "bigint", NotNull: true, Default: "identity always", HasDefault: true},
		}, Checks: []string{"CHECK (subscribers >= 0)"}},
		{Name: "videos", Columns: []Column{
			{Name: "id", Type: "bigint", NotNull: true, Default: "identity by default", HasDefault: true, PK: 1},
			{Name: "channel_id", Type: "text"},
			{Name: "embedding", Type: "vector(768)"},
			{Name: "added_at", Type: "timestamp with time zone", NotNull: true, Default: "now()", HasDefault: true},
			{Name: "title_lc", Type: "text", Default: "generated: lower(title)", HasDefault: true},
		}, ForeignKeys: []ForeignKey{
			{Columns: []string{"channel_id"}, RefTable: "channels", RefColumns: []string{"id"}, OnUpdate: "NO ACTION", OnDelete: "SET NULL"},
		}, Indexes: []Index{
			{Name: "idx_videos_channel", Columns: []string{"channel_id", "added_at"}},
			{Name: "idx_videos_lower", Columns: []string{"lower(id)"}, Unique: true, Where: "channel_id IS NOT NULL"},
		}},
	}, Views: []View{{Name: "active_videos", Kind: "view"}, {Name: "channel_stats", Kind: "materialized view"}}}
	if !reflect.DeepEqual(s, want) {
		t.Errorf("schema = %+v\nwant %+v", s, want)
	}
}

func TestPostgresDefault(t *testing.T) {
	for _, c := range []struct {
		def                 sql.NullString
		identity, generated string
		want                string
		has                 bool
	}{
		{sql.NullString{}, "", "", "", false},
		{sql.NullString{String: "0", Valid: true}, "", "", "0", true},
		{sql.NullString{}, "a", "", "identity always", true},
		{sql.NullString{}, "d", "", "identity by default", true},
		{sql.NullString{String: "a + b", Valid: true}, "", "s", "generated: a + b", true},
	} {
		if got, has := pgDefault(c.def, c.identity, c.generated); got != c.want || has != c.has {
			t.Errorf("pgDefault(%v, %q, %q) = %q, %v; want %q, %v", c.def, c.identity, c.generated, got, has, c.want, c.has)
		}
	}
}

func TestPostgresQueryErrors(t *testing.T) {
	for _, c := range []struct {
		q   string
		arg []any
	}{
		{qPostgresTables, nil},
		{qPostgresColumns, args("videos")},
		{qPostgresForeignKeys, args("videos")},
		{qPostgresIndexes, args("videos")},
		{qPostgresChecks, args("videos")},
		{qPostgresViews, nil},
	} {
		f := postgresFixture()
		f.failOn(c.q, c.arg...)
		if _, err := Describe(context.Background(), f.open(t), Postgres); err == nil || !strings.Contains(err.Error(), "boom") {
			t.Errorf("%.40s %v: err = %v, want boom", c.q, c.arg, err)
		}
	}
}

func TestPostgresAction(t *testing.T) {
	for code, want := range map[string]string{
		"a": "NO ACTION", "r": "RESTRICT", "c": "CASCADE", "n": "SET NULL", "d": "SET DEFAULT", "?": "?",
	} {
		if got := pgAction(code); got != want {
			t.Errorf("pgAction(%q) = %q, want %q", code, got, want)
		}
	}
}
