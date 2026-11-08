package lib

import "database/sql"

// Row lives in a go.modules module: read like go.dir.
type Row struct{ ID int64 }

func get(row *sql.Row) (r Row, err error) {
	err = row.Scan(&r.ID)
	return
}
