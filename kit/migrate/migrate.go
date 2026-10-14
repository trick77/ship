// Package migrate applies numbered SQL files to a database/sql database, exactly once each, in order.
//
// The bookkeeping table is schema_migrations(name TEXT PRIMARY KEY, applied_at), keyed by file name, so a repo that
// already runs that table switches with no data step. A goose database converts once with AdoptGoose.
//
// Files are *.sql at the root of the given fs.FS, ordered by their numeric prefix (0001_init.sql, 00002_runs.sql).
// Each runs in its own transaction together with its bookkeeping row, so a failure leaves it unrecorded and it runs
// again next time. A file whose first line is the no-transaction marker runs statement by statement outside one
// (TimescaleDB refuses continuous aggregates inside a transaction block).
//
// Concurrent replicas are serialised by the dialect's lock (a Postgres session advisory lock), held on one dedicated
// connection for the whole run, because a session lock belongs to one session and a pool hands out a different
// connection per call.
//
// A pgx program opens a dedicated *sql.DB for the run (github.com/jackc/pgx/v5/stdlib: OpenDB on a pgx.ConnConfig,
// or OpenDBFromPool on a separate small pool), passes it with Postgres(lockKey) and closes it afterwards: session
// state (the advisory lock, a SET in a no-transaction file) then never returns to the app's pool.
package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Table is the bookkeeping table.
const Table = "schema_migrations"

// NoTxMarker is the first line that runs a file outside a transaction.
const NoTxMarker = "-- migrate:no-transaction"

// Dialect is what differs between database engines.
type Dialect struct {
	// Placeholder renders the n-th (1-based) bind parameter: $1 for Postgres, ? for SQLite.
	Placeholder func(n int) string
	// CreateTable creates Table if it does not exist.
	CreateTable string
	// TableExists is a query taking one table name and returning one boolean-ish column.
	TableExists string
	// Lock and Unlock serialise migrators across processes on conn; nil for an engine that needs none.
	Lock, Unlock func(ctx context.Context, conn *sql.Conn) error
}

// SQLite needs no lock: one file, one writer, and its own locking makes a second migrator wait.
var SQLite = Dialect{
	Placeholder: func(int) string { return "?" },
	CreateTable: `CREATE TABLE IF NOT EXISTS ` + Table + ` (
		name TEXT PRIMARY KEY,
		applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
	)`,
	TableExists: `SELECT count(*) > 0 FROM sqlite_master WHERE type = 'table' AND name = ?`,
}

// Postgres serialises migrators with pg_advisory_lock(lockKey). The key is arbitrary but fixed per app: advisory
// locks are namespaced by value alone, so every replica must use the same one and other apps a different one.
func Postgres(lockKey int64) Dialect {
	return Dialect{
		Placeholder: func(n int) string { return "$" + strconv.Itoa(n) },
		CreateTable: `CREATE TABLE IF NOT EXISTS ` + Table + ` (
			name       TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		TableExists: `SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = current_schema() AND table_name = $1)`,
		Lock: func(ctx context.Context, c *sql.Conn) error {
			_, err := c.ExecContext(ctx, `SELECT pg_advisory_lock($1)`, lockKey)
			return err
		},
		Unlock: func(ctx context.Context, c *sql.Conn) error {
			_, err := c.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, lockKey)
			return err
		},
	}
}

// Options configures a run.
type Options struct {
	Dialect Dialect
	// Log gets one line per applied file; nil is slog.Default.
	Log *slog.Logger
	// NoTxMarker overrides the package's marker, so a repo with its own spelling ("-- app:no-transaction") need
	// not rewrite applied files.
	NoTxMarker string
	// LockTimeout bounds the wait for another migrator's lock: pg_advisory_lock blocks forever, and a wedged holder
	// would otherwise hang every other replica with no log line. Default 5 minutes, generous because a first deploy
	// applies the whole schema.
	LockTimeout time.Duration
}

func (o *Options) defaults() {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.NoTxMarker == "" {
		o.NoTxMarker = NoTxMarker
	}
	if o.LockTimeout == 0 {
		o.LockTimeout = 5 * time.Minute
	}
}

// Migration is one file.
type Migration struct {
	Version int64
	Name    string
	body    string
}

// Files lists fsys's *.sql files in version order. A file without a numeric prefix, or two with the same version,
// is an error: the order would be a guess.
func Files(fsys fs.FS) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}
	var out []Migration
	seen := map[int64]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || path.Ext(name) != ".sql" {
			continue
		}
		digits := name[:len(name)-len(strings.TrimLeft(name, "0123456789"))]
		v, err := strconv.ParseInt(digits, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("migration %s: no numeric version prefix", name)
		}
		if prev, dup := seen[v]; dup {
			return nil, fmt.Errorf("migrations %s and %s share version %d", prev, name, v)
		}
		seen[v] = name
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		out = append(out, Migration{Version: v, Name: name, body: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// Up applies every pending file and returns the names it applied, none when the schema is current.
func Up(ctx context.Context, db *sql.DB, fsys fs.FS, o Options) ([]string, error) {
	o.defaults()
	files, err := Files(fsys)
	if err != nil {
		return nil, err
	}
	var applied []string
	err = locked(ctx, db, o, func(conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, o.Dialect.CreateTable); err != nil {
			return fmt.Errorf("create %s: %w", Table, err)
		}
		done, err := appliedNames(ctx, conn)
		if err != nil {
			return err
		}
		insert := insertSQL(o.Dialect)
		for _, m := range files {
			if done[m.Name] {
				continue
			}
			start := time.Now()
			if err := apply(ctx, conn, m, insert, o.NoTxMarker); err != nil {
				return fmt.Errorf("apply %s: %w", m.Name, err)
			}
			applied = append(applied, m.Name)
			o.Log.InfoContext(ctx, "Migration applied", "file", m.Name, "took", time.Since(start).String())
		}
		return nil
	})
	return applied, err
}

// Pending lists the files not yet applied, for a startup gate that refuses to serve on a schema that is behind (a
// deploy that skipped its migrate step should fail at boot with a name, not at the first query).
func Pending(ctx context.Context, db *sql.DB, fsys fs.FS, o Options) ([]string, error) {
	o.defaults()
	files, err := Files(fsys)
	if err != nil {
		return nil, err
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	done := map[string]bool{}
	exists, err := tableExists(ctx, conn, o.Dialect, Table)
	if err != nil {
		return nil, err
	}
	if exists {
		if done, err = appliedNames(ctx, conn); err != nil {
			return nil, err
		}
	}
	var pending []string
	for _, m := range files {
		if !done[m.Name] {
			pending = append(pending, m.Name)
		}
	}
	return pending, nil
}

// GooseTable is the version table goose keeps.
const GooseTable = "goose_db_version"

// AdoptGoose records every migration goose applied as applied here, so Up continues where goose stopped. Run it once
// before the first Up on a goose database; it is idempotent and a no-op without a goose table. goose's table is
// left as it was, so rolling back to the goose build still works.
//
// goose logs every up and down as a row; a version's latest row says whether it is applied. An applied version with
// no file in fsys is an error: the history would be silently cut.
func AdoptGoose(ctx context.Context, db *sql.DB, fsys fs.FS, o Options) (int, error) {
	o.defaults()
	files, err := Files(fsys)
	if err != nil {
		return 0, err
	}
	byVersion := make(map[int64]string, len(files))
	for _, m := range files {
		byVersion[m.Version] = m.Name
	}
	adopted := 0
	err = locked(ctx, db, o, func(conn *sql.Conn) error {
		exists, err := tableExists(ctx, conn, o.Dialect, GooseTable)
		if err != nil || !exists {
			return err
		}
		state, err := gooseState(ctx, conn)
		if err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, o.Dialect.CreateTable); err != nil {
			return fmt.Errorf("create %s: %w", Table, err)
		}
		done, err := appliedNames(ctx, conn)
		if err != nil {
			return err
		}
		var names []string
		for v, ok := range state {
			if !ok || v == 0 { // version 0 is goose's own bootstrap row
				continue
			}
			name, found := byVersion[v]
			if !found {
				return fmt.Errorf("goose applied version %d, which has no migration file", v)
			}
			if !done[name] {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		insert := insertSQL(o.Dialect)
		for _, n := range names {
			if _, err := tx.ExecContext(ctx, insert, n); err != nil {
				return fmt.Errorf("adopt %s: %w", n, err)
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		adopted = len(names)
		if adopted > 0 {
			o.Log.InfoContext(ctx, "Adopted goose migrations", "count", adopted)
		}
		return nil
	})
	return adopted, err
}

// gooseState is each version's latest is_applied.
func gooseState(ctx context.Context, conn *sql.Conn) (map[int64]bool, error) {
	rows, err := conn.QueryContext(ctx, `SELECT version_id, is_applied FROM `+GooseTable+` ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", GooseTable, err)
	}
	defer func() { _ = rows.Close() }()
	state := map[int64]bool{}
	for rows.Next() {
		var v int64
		var ok bool
		if err := rows.Scan(&v, &ok); err != nil {
			return nil, err
		}
		state[v] = ok
	}
	return state, rows.Err()
}

// locked runs fn on one dedicated connection under the dialect's lock.
func locked(ctx context.Context, db *sql.DB, o Options, fn func(*sql.Conn) error) error {
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("acquire migration connection: %w", err)
	}
	defer func() { _ = conn.Close() }()
	if o.Dialect.Lock != nil {
		lockCtx, cancel := context.WithTimeout(ctx, o.LockTimeout)
		err := o.Dialect.Lock(lockCtx, conn)
		cancel()
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("timed out after %s waiting for the migration lock: another migrator is running or "+
				"wedged holding it: %w", o.LockTimeout, err)
		}
		if err != nil {
			return fmt.Errorf("acquire migration lock: %w", err)
		}
		if o.Dialect.Unlock != nil {
			// Released explicitly so the next replica proceeds at once; best effort, since a dead session drops
			// its locks anyway.
			defer func() { _ = o.Dialect.Unlock(context.WithoutCancel(ctx), conn) }()
		}
	}
	return fn(conn)
}

// insertSQL records one applied file. Only the dialect's placeholder is spliced in, never a value.
func insertSQL(d Dialect) string {
	return `INSERT INTO ` + Table + ` (name) VALUES (` + d.Placeholder(1) + `)` //nolint:gosec // a constant and a placeholder
}

func tableExists(ctx context.Context, conn *sql.Conn, d Dialect, table string) (bool, error) {
	var ok bool
	if err := conn.QueryRowContext(ctx, d.TableExists, table).Scan(&ok); err != nil {
		return false, fmt.Errorf("look up %s: %w", table, err)
	}
	return ok, nil
}

func appliedNames(ctx context.Context, conn *sql.Conn) (map[string]bool, error) {
	rows, err := conn.QueryContext(ctx, `SELECT name FROM `+Table)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", Table, err)
	}
	defer func() { _ = rows.Close() }()
	done := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		done[n] = true
	}
	return done, rows.Err()
}

func apply(ctx context.Context, conn *sql.Conn, m Migration, insert, marker string) error {
	body, noTx := upSection(m.body, marker)
	if noTx {
		// Statement by statement: a multi-statement body goes over the simple-query protocol, which Postgres wraps
		// in an implicit transaction anyway. A partial failure leaves the file unrecorded, so it is retried; its
		// statements must be idempotent.
		for _, stmt := range splitStatements(body) {
			if _, err := conn.ExecContext(ctx, stmt); err != nil {
				return err
			}
		}
		_, err := conn.ExecContext(ctx, insert, m.Name)
		return err
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if strings.TrimSpace(body) != "" {
		// No args: both pgx and the SQLite drivers run a multi-statement body as is.
		if _, err := tx.ExecContext(ctx, body); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, insert, m.Name); err != nil {
		return err
	}
	return tx.Commit()
}

// upSection is what to execute and whether outside a transaction. A goose-annotated file (a repo coming from goose)
// runs its "-- +goose Up" section only, never the Down below it; "-- +goose NO TRANSACTION" is honoured like the
// marker.
func upSection(body, marker string) (string, bool) {
	noTx := strings.HasPrefix(strings.TrimSpace(body), marker)
	if i := strings.Index(body, "-- +goose Up"); i >= 0 {
		head := body[:i]
		body = body[i+len("-- +goose Up"):]
		if j := strings.Index(body, "-- +goose Down"); j >= 0 {
			body = body[:j]
		}
		noTx = noTx || strings.Contains(head, "-- +goose NO TRANSACTION")
	}
	return body, noTx
}
