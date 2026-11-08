package bad

import "database/sql"

// B is a row of bs.
//
// Table: bs
type B struct{ ID int64 }

func (b *B) scan(row *sql.Row) error { return row.Scan(&b.ID) }
