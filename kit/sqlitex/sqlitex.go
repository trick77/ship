// Package sqlitex is the SQLite DSN every service spelled by hand: WAL, a busy timeout and foreign keys, as the
// github.com/ncruces/go-sqlite3 driver reads them.
package sqlitex

import (
	"net/url"
	"strings"
)

// DSN is the file DSN for path, opened with WAL (readers never block the one writer), a 10 s busy timeout (a locked
// database waits instead of failing at once) and foreign keys on (off by SQLite's default). extra are further
// parameters as written after the ?, e.g. "_txlock=immediate" (a write transaction takes its lock at BEGIN, never
// upgrading mid-transaction into SQLITE_BUSY).
func DSN(path string, extra ...string) string {
	params := append([]string{"_pragma=journal_mode(wal)", "_pragma=busy_timeout(10000)", "_pragma=foreign_keys(on)"}, extra...)
	return "file:" + url.PathEscape(path) + "?" + strings.Join(params, "&")
}
