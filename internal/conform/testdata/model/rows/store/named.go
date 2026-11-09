package store

import (
	"database/sql"

	"example.com/rows/scanx"
)

// H is scanned through this module's package Scan func: its dest ...any signature decides, no import needed.
type H struct{ F int }

// M is scanned through a method expression.
type M struct{ F int }

func named(row *sql.Row) (h H, m M, err error) {
	if err = scanx.Scan(row, &h.F); err != nil {
		return
	}
	err = (*sql.Row).Scan(row, &m.F)
	return
}
