package migrate

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func files(m map[string]string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for k, v := range m {
		fsys[k] = &fstest.MapFile{Data: []byte(v)}
	}
	return fsys
}

var quiet = slog.New(slog.DiscardHandler)

func TestFilesOrderAndFaults(t *testing.T) {
	got, err := Files(files(map[string]string{
		"0010_ten.sql": "", "0002_two.sql": "", "00001_one.sql": "", "README.md": "", "embed.go": "",
	}))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, m := range got {
		names = append(names, m.Name)
	}
	if want := []string{"00001_one.sql", "0002_two.sql", "0010_ten.sql"}; !reflect.DeepEqual(names, want) {
		t.Errorf("order = %v, want %v", names, want)
	}
	for name, fsys := range map[string]fstest.MapFS{
		"no prefix": files(map[string]string{"init.sql": ""}),
		"duplicate": files(map[string]string{"1_a.sql": "", "01_b.sql": ""}),
	} {
		if _, err := Files(fsys); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if _, err := Files(fstest.MapFS{"x": {Mode: 0}}); err != nil {
		t.Errorf("non-sql file: %v", err)
	}
}

func TestUpPostgres(t *testing.T) {
	db := newFake()
	fsys := files(map[string]string{
		"0001_init.sql":  "CREATE TABLE a (id int); CREATE TABLE b (id int);",
		"0002_agg.sql":   "-- app:no-transaction\nCREATE MATERIALIZED VIEW v AS SELECT 1; SELECT 'x;y';",
		"0003_empty.sql": "-- nothing yet\n",
	})
	o := Options{Dialect: Postgres(42), Log: quiet, NoTxMarker: "-- app:no-transaction"}
	applied, err := Up(context.Background(), db.open(t), fsys, o)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"0001_init.sql", "0002_agg.sql", "0003_empty.sql"}; !reflect.DeepEqual(applied, want) {
		t.Errorf("applied = %v", applied)
	}
	got := db.statements()
	want := []string{
		"SELECT pg_advisory_lock($1)",
		"CREATE TABLE IF NOT EXISTS schema_migrations ( name TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now() )",
		"SELECT name FROM schema_migrations",
		"BEGIN",
		"CREATE TABLE a (id int); CREATE TABLE b (id int);",
		"INSERT INTO schema_migrations (name) VALUES ($1)",
		"COMMIT",
		"-- app:no-transaction CREATE MATERIALIZED VIEW v AS SELECT 1",
		"SELECT 'x;y'",
		"INSERT INTO schema_migrations (name) VALUES ($1)",
		"BEGIN",
		"-- nothing yet",
		"INSERT INTO schema_migrations (name) VALUES ($1)",
		"COMMIT",
		"SELECT pg_advisory_unlock($1)",
	}
	got = without(got, "ROLLBACK")
	want = without(want, "ROLLBACK")
	if !reflect.DeepEqual(got, want) {
		t.Errorf("statements:\n got %q\nwant %q", got, want)
	}

	// Idempotent: a second run applies nothing.
	again, err := Up(context.Background(), db.open(t), fsys, o)
	if err != nil || len(again) != 0 {
		t.Errorf("second Up = %v, %v", again, err)
	}
	pending, err := Pending(context.Background(), db.open(t), fsys, o)
	if err != nil || len(pending) != 0 {
		t.Errorf("Pending after Up = %v, %v", pending, err)
	}
}

func without(s []string, drop string) []string {
	var out []string
	for _, v := range s {
		if v != drop {
			out = append(out, v)
		}
	}
	return out
}

func TestUpFailureLeavesFileUnrecorded(t *testing.T) {
	db := newFake()
	db.failOn = "BROKEN"
	fsys := files(map[string]string{"1_ok.sql": "SELECT 1", "2_bad.sql": "BROKEN", "3_later.sql": "SELECT 3"})
	applied, err := Up(context.Background(), db.open(t), fsys, Options{Dialect: SQLite, Log: quiet})
	if err == nil || !strings.Contains(err.Error(), "apply 2_bad.sql") {
		t.Fatalf("err = %v", err)
	}
	if !reflect.DeepEqual(applied, []string{"1_ok.sql"}) || !reflect.DeepEqual(db.applied, []string{"1_ok.sql"}) {
		t.Errorf("applied = %v, recorded = %v", applied, db.applied)
	}
	for _, s := range db.statements() {
		if strings.Contains(s, "advisory") {
			t.Error("SQLite took a Postgres lock")
		}
		if strings.Contains(s, "INSERT") && strings.Contains(s, "$1") {
			t.Error("SQLite got a $1 placeholder")
		}
	}
	pending, err := Pending(context.Background(), db.open(t), fsys, Options{Dialect: SQLite})
	if err != nil || !reflect.DeepEqual(pending, []string{"2_bad.sql", "3_later.sql"}) {
		t.Errorf("Pending = %v, %v", pending, err)
	}

	// No-transaction file failing mid-way stays unrecorded too.
	db2 := newFake()
	db2.failOn = "BROKEN"
	_, err = Up(context.Background(), db2.open(t), files(map[string]string{"1.sql": NoTxMarker + "\nSELECT 1; BROKEN;"}),
		Options{Dialect: SQLite, Log: quiet})
	if err == nil || len(db2.applied) != 0 {
		t.Errorf("no-tx failure: err %v, applied %v", err, db2.applied)
	}
}

func TestPendingOnEmptyDatabase(t *testing.T) {
	db := newFake()
	pending, err := Pending(context.Background(), db.open(t), files(map[string]string{"1_a.sql": "", "2_b.sql": ""}),
		Options{Dialect: Postgres(1)})
	if err != nil || len(pending) != 2 {
		t.Errorf("Pending = %v, %v", pending, err)
	}
	db.existsErr = true
	if _, err := Pending(context.Background(), db.open(t), files(map[string]string{"1_a.sql": ""}), Options{Dialect: SQLite}); err == nil {
		t.Error("exists failure swallowed")
	}
	if _, err := Pending(context.Background(), db.open(t), files(map[string]string{"x.sql": ""}), Options{Dialect: SQLite}); err == nil {
		t.Error("bad file name swallowed")
	}
}

func TestLockErrors(t *testing.T) {
	fsys := files(map[string]string{"1.sql": ""})
	db := newFake()
	db.lockErr = context.DeadlineExceeded
	if _, err := Up(context.Background(), db.open(t), fsys, Options{Dialect: Postgres(1), LockTimeout: time.Millisecond}); err == nil ||
		!strings.Contains(err.Error(), "timed out") {
		t.Errorf("timeout err = %v", err)
	}
	db.lockErr = errors.New("conn reset")
	if _, err := Up(context.Background(), db.open(t), fsys, Options{Dialect: Postgres(1)}); err == nil ||
		!strings.Contains(err.Error(), "acquire migration lock") {
		t.Errorf("lock err = %v", err)
	}
	if _, err := Up(context.Background(), db.open(t), files(map[string]string{"x.sql": ""}), Options{Dialect: SQLite}); err == nil {
		t.Error("bad file name accepted")
	}
	db3 := newFake()
	db3.failOn = "CREATE TABLE IF NOT EXISTS"
	if _, err := Up(context.Background(), db3.open(t), fsys, Options{Dialect: SQLite}); err == nil {
		t.Error("create-table failure swallowed")
	}
}

func TestGooseFileRunsUpSectionOnly(t *testing.T) {
	db := newFake()
	fsys := files(map[string]string{
		"00001_schema.sql": "-- header\n-- +goose Up\nCREATE TABLE t (x int);\n-- +goose Down\nDROP TABLE t;\n",
		"00002_notx.sql":   "-- +goose NO TRANSACTION\n-- +goose Up\nCREATE INDEX i ON t (x);\n",
	})
	if _, err := Up(context.Background(), db.open(t), fsys, Options{Dialect: SQLite, Log: quiet}); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(db.statements(), "\n")
	if strings.Contains(joined, "DROP TABLE") {
		t.Error("Down section executed")
	}
	if !strings.Contains(joined, "CREATE INDEX i ON t (x)") {
		t.Errorf("no-tx goose file not run: %s", joined)
	}
}

func TestAdoptGoose(t *testing.T) {
	fsys := files(map[string]string{"00001_schema.sql": "", "00002_runs.sql": "", "00003_cone.sql": "", "00004_new.sql": ""})
	db := newFake()
	o := Options{Dialect: SQLite, Log: quiet}

	// No goose table: nothing to adopt.
	if n, err := AdoptGoose(context.Background(), db.open(t), fsys, o); err != nil || n != 0 {
		t.Fatalf("no goose table: %d, %v", n, err)
	}

	db.tables[GooseTable] = true
	// Version 3 applied then rolled back; 0 is goose's bootstrap row.
	db.goose = [][2]any{{int64(0), true}, {int64(1), true}, {int64(2), true}, {int64(3), true}, {int64(3), false}}
	n, err := AdoptGoose(context.Background(), db.open(t), fsys, o)
	if err != nil || n != 2 {
		t.Fatalf("AdoptGoose = %d, %v", n, err)
	}
	if want := []string{"00001_schema.sql", "00002_runs.sql"}; !reflect.DeepEqual(db.applied, want) {
		t.Errorf("adopted = %v", db.applied)
	}
	// Idempotent, and Up then applies only what goose had not.
	if n, err := AdoptGoose(context.Background(), db.open(t), fsys, o); err != nil || n != 0 {
		t.Errorf("second AdoptGoose = %d, %v", n, err)
	}
	applied, err := Up(context.Background(), db.open(t), fsys, o)
	if err != nil || !reflect.DeepEqual(applied, []string{"00003_cone.sql", "00004_new.sql"}) {
		t.Errorf("Up after adopt = %v, %v", applied, err)
	}

	// A goose version with no file is an error, not a silent skip.
	db2 := newFake()
	db2.tables[GooseTable] = true
	db2.goose = [][2]any{{int64(9), true}}
	if _, err := AdoptGoose(context.Background(), db2.open(t), fsys, o); err == nil ||
		!strings.Contains(err.Error(), "version 9") {
		t.Errorf("missing file err = %v", err)
	}
	if _, err := AdoptGoose(context.Background(), db2.open(t), files(map[string]string{"x.sql": ""}), o); err == nil {
		t.Error("bad file name accepted")
	}
	db2.failOn = "CREATE TABLE IF NOT EXISTS"
	db2.goose = nil
	if _, err := AdoptGoose(context.Background(), db2.open(t), fsys, o); err == nil {
		t.Error("create-table failure swallowed")
	}
}

func TestSplitStatements(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want []string
	}{
		{"SELECT 1; SELECT 2;", []string{"SELECT 1", "SELECT 2"}},
		{"SELECT 'a;b'; SELECT 'it''s;'", []string{"SELECT 'a;b'", "SELECT 'it''s;'"}},
		{"DO $$ BEGIN x; END $$; SELECT 1", []string{"DO $$ BEGIN x; END $$", "SELECT 1"}},
		{"DO $f$ a; $f$;", []string{"DO $f$ a; $f$"}},
		{"SELECT $1; SELECT a$b", []string{"SELECT $1", "SELECT a$b"}},
		{"-- c; d\nSELECT 1; /* x; y */ SELECT 2", []string{"-- c; d\nSELECT 1", "/* x; y */ SELECT 2"}},
		{"SELECT 1; -- trailing\n", []string{"SELECT 1"}},
		{"SELECT 1; /* only */ ; /* open", []string{"SELECT 1"}},
		{"$", []string{"$"}},
	} {
		if got := splitStatements(tt.in); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("splitStatements(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
