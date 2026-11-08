package store

import "database/sql"

// Rec is scanned by its own method.
type Rec struct{ ID int64 }

func (r *Rec) scan(row *sql.Row) error { return row.Scan(&r.ID) }
