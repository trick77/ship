package migrate

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
)

// fakeDB is just enough of a database for the runner: it knows the two bookkeeping tables, honours transactions for
// the rows it keeps, and records every statement so a test can assert order and transaction boundaries.
type fakeDB struct {
	mu        sync.Mutex
	log       []string
	tables    map[string]bool
	applied   []string
	goose     [][2]any // version_id, is_applied in id order
	failOn    string   // a statement containing this fails
	lockErr   error
	existsErr bool
}

func newFake() *fakeDB { return &fakeDB{tables: map[string]bool{}} }

func (f *fakeDB) open(t *testing.T) *sql.DB {
	t.Helper()
	db := sql.OpenDB(f)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func (f *fakeDB) Connect(context.Context) (driver.Conn, error) { return &fakeConn{db: f}, nil }
func (f *fakeDB) Driver() driver.Driver                        { return fakeDriver{} }

type fakeDriver struct{}

func (fakeDriver) Open(string) (driver.Conn, error) { return nil, errors.New("use the connector") }

func (f *fakeDB) record(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, strings.Join(strings.Fields(s), " "))
}

func (f *fakeDB) statements() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.log...)
}

type fakeConn struct {
	db *fakeDB
	tx *fakeTx
}

func (c *fakeConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare unsupported")
}
func (c *fakeConn) Close() error { return nil }
func (c *fakeConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *fakeConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	c.db.record("BEGIN")
	c.tx = &fakeTx{c: c}
	return c.tx, nil
}

type fakeTx struct {
	c      *fakeConn
	staged []string
}

func (t *fakeTx) Commit() error {
	t.c.db.record("COMMIT")
	t.c.db.mu.Lock()
	t.c.db.applied = append(t.c.db.applied, t.staged...)
	t.c.db.mu.Unlock()
	t.c.tx = nil
	return nil
}

func (t *fakeTx) Rollback() error {
	t.c.db.record("ROLLBACK")
	t.c.tx = nil
	return nil
}

func (c *fakeConn) ExecContext(_ context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	c.db.record(q)
	d := c.db
	switch {
	case d.failOn != "" && strings.Contains(q, d.failOn):
		return nil, errors.New("boom: " + d.failOn)
	case strings.Contains(q, "pg_advisory_lock("):
		return driver.RowsAffected(0), d.lockErr
	case strings.HasPrefix(strings.TrimSpace(q), "CREATE TABLE IF NOT EXISTS "+Table):
		d.mu.Lock()
		d.tables[Table] = true
		d.mu.Unlock()
	case strings.HasPrefix(q, "INSERT INTO "+Table):
		name := args[0].Value.(string)
		if c.tx != nil {
			c.tx.staged = append(c.tx.staged, name)
		} else {
			d.mu.Lock()
			d.applied = append(d.applied, name)
			d.mu.Unlock()
		}
	}
	return driver.RowsAffected(1), nil
}

func (c *fakeConn) QueryContext(_ context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	c.db.record(q)
	d := c.db
	d.mu.Lock()
	defer d.mu.Unlock()
	switch {
	case strings.Contains(q, "information_schema.tables") || strings.Contains(q, "sqlite_master"):
		if d.existsErr {
			return nil, errors.New("exists failed")
		}
		return &fakeRows{cols: []string{"exists"}, rows: [][]driver.Value{{d.tables[args[0].Value.(string)]}}}, nil
	case strings.HasPrefix(q, "SELECT name FROM "+Table):
		r := &fakeRows{cols: []string{"name"}}
		for _, n := range d.applied {
			r.rows = append(r.rows, []driver.Value{n})
		}
		return r, nil
	case strings.HasPrefix(q, "SELECT version_id, is_applied FROM "+GooseTable):
		r := &fakeRows{cols: []string{"version_id", "is_applied"}}
		for _, g := range d.goose {
			r.rows = append(r.rows, []driver.Value{g[0], g[1]})
		}
		return r, nil
	}
	return nil, errors.New("unexpected query: " + q)
}

type fakeRows struct {
	cols []string
	rows [][]driver.Value
}

func (r *fakeRows) Columns() []string { return r.cols }
func (r *fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if len(r.rows) == 0 {
		return io.EOF
	}
	copy(dest, r.rows[0])
	r.rows = r.rows[1:]
	return nil
}
