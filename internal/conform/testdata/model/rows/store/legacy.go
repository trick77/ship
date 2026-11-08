package store

import "database/sql"

// legacy is unexported: a lowercase name dodges nothing.
type legacy struct {
	ID   int64
	Name string
}

// legacyDest builds the dest slice a scan spreads.
func legacyDest(l *legacy) []any { return []any{&l.ID, &l.Name} }

func getLegacy(rows *sql.Rows) (legacy, error) {
	var l legacy
	err := rows.Scan(legacyDest(&l)...)
	return l, err
}

// appended is filled through append.
type appended struct{ X int }

func getAppended(rows *sql.Rows) (appended, error) {
	var a appended
	dest := []any{}
	dest = append(dest, &a.X)
	return a, rows.Scan(dest...)
}
