package sqlitex

import "testing"

func TestDSN(t *testing.T) {
	const base = "?_pragma=busy_timeout(10000)&_pragma=journal_mode(wal)&_pragma=foreign_keys(on)"
	for _, tc := range []struct {
		path  string
		extra []string
		want  string
	}{
		{"data/app.db", nil, "file:data%2Fapp.db" + base},
		{"/var/lib/app/a b.db", []string{"_txlock=immediate"}, "file:%2Fvar%2Flib%2Fapp%2Fa%20b.db" + base + "&_txlock=immediate"},
	} {
		if got := DSN(tc.path, tc.extra...); got != tc.want {
			t.Errorf("DSN(%q, %v) = %q, want %q", tc.path, tc.extra, got, tc.want)
		}
	}
}
