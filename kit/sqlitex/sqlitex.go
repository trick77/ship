// Package sqlitex is the SQLite DSN every service spelled by hand: a busy timeout, WAL and foreign keys, as the
// github.com/ncruces/go-sqlite3 driver reads them.
package sqlitex

import (
	"net/url"
	"strings"
)

// DSN is the file DSN for path, opened with a 10 s busy timeout (a locked database waits instead of failing at
// once), WAL (readers never block the one writer) and foreign keys on (off by SQLite's default). The busy timeout
// comes first: journal_mode(wal) reads the file header, and under another connection's lock that would otherwise
// fail the connection open under load; foreign_keys(on) is a connection flag and never waits. extra are further
// parameters as written after the ?, e.g. "_txlock=immediate" (a write transaction takes its lock at BEGIN, never
// upgrading mid-transaction into SQLITE_BUSY).
func DSN(path string, extra ...string) string {
	params := append([]string{"_pragma=busy_timeout(10000)", "_pragma=journal_mode(wal)", "_pragma=foreign_keys(on)"}, extra...)
	return "file:" + url.PathEscape(path) + "?" + strings.Join(params, "&")
}
