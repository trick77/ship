package schemadoc

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

// fakeDB answers catalog queries with canned rows keyed by the query text and its arguments, the way
// kit/migrate's fake does: kit has no SQL driver of its own. It proves Describe's assembly of what a catalog
// returns, not the SQL itself; that runs for real in every consumer's own schema test.
type fakeDB struct {
	rows map[string][][]driver.Value
	fail map[string]bool
	seen []string
}

func newFake() *fakeDB { return &fakeDB{rows: map[string][][]driver.Value{}, fail: map[string]bool{}} }

func fakeKey(q string, args []any) string {
	return strings.Join(strings.Fields(q), " ") + " " + fmt.Sprint(args...)
}

// on sets the rows q returns for args.
func (f *fakeDB) on(q string, args []any, rows ...[]driver.Value) {
	f.rows[fakeKey(q, args)] = rows
}

// failOn makes q with args fail.
func (f *fakeDB) failOn(q string, args ...any) { f.fail[fakeKey(q, args)] = true }

func (f *fakeDB) open(t *testing.T) *sql.DB {
	t.Helper()
	db := sql.OpenDB(f)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func (f *fakeDB) Connect(context.Context) (driver.Conn, error) { return fakeConn{f}, nil }
func (f *fakeDB) Driver() driver.Driver                        { return fakeDriver{} }

type fakeDriver struct{}

func (fakeDriver) Open(string) (driver.Conn, error) { return nil, errors.New("use the connector") }

type fakeConn struct{ db *fakeDB }

func (fakeConn) Prepare(string) (driver.Stmt, error) { return nil, errors.New("prepare unsupported") }
func (fakeConn) Close() error                        { return nil }
func (fakeConn) Begin() (driver.Tx, error)           { return nil, errors.New("read only") }

func (c fakeConn) QueryContext(_ context.Context, q string, named []driver.NamedValue) (driver.Rows, error) {
	args := make([]any, len(named))
	for i, n := range named {
		args[i] = n.Value
	}
	k := fakeKey(q, args)
	c.db.seen = append(c.db.seen, k)
	if c.db.fail[k] {
		return nil, errors.New("boom")
	}
	rows, ok := c.db.rows[k]
	if !ok {
		return nil, errors.New("unexpected query: " + k)
	}
	return &fakeRows{rows: rows}, nil
}

type fakeRows struct{ rows [][]driver.Value }

func (r *fakeRows) Columns() []string {
	if len(r.rows) == 0 {
		return nil
	}
	return make([]string, len(r.rows[0]))
}
func (r *fakeRows) Close() error { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	if len(r.rows) == 0 {
		return io.EOF
	}
	copy(dest, r.rows[0])
	r.rows = r.rows[1:]
	return nil
}

// row is one canned row.
func row(v ...driver.Value) []driver.Value { return v }

// args is a query's arguments.
func args(v ...any) []any { return v }
