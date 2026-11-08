package tools

import "database/sql"

// T is in a module ship.yaml does not list: never read.
type T struct{ ID int64 }

func get(row *sql.Row) (t T, err error) {
	err = row.Scan(&t.ID)
	return
}
