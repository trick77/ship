package bad

import (
	"bufio"
	"database/sql"
)

// B is a row of bs.
//
// Table: bs
type B struct{ ID int64 }

func (b *B) scan(row *sql.Row) error  { return row.Scan(&b.ID) }
func (b *B) scanP(row *sql.Row) error { return (row.Scan)(&b.ID) }

// lines reads no row: bufio's Scan is no database code.
func lines(s *bufio.Scanner) {
	for s.Scan() {
	}
}
