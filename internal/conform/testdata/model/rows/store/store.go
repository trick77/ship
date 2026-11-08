package store

import "database/sql"

func video(row *sql.Row) (Video, error) {
	var v Video
	err := row.Scan(&v.ID, &v.Title)
	return v, err
}

// A range variable.
func counts(rows *sql.Rows, cs []Count) error {
	for _, c := range cs {
		if err := rows.Scan(&c.N); err != nil {
			return err
		}
	}
	return nil
}

// A named result.
func tag(row *sql.Row) (t Tag, err error) {
	err = row.Scan(&t.ID)
	return
}

func newNote() *Note { return &Note{} }

// A function's result.
func note(row *sql.Row) (*Note, error) {
	n := newNote()
	return n, row.Scan(&n.ID)
}

// A receiver.
func (c *Channel) scan(row *sql.Row) error { return row.Scan(&c.ID) }

// A function-local type cannot move: exempt.
func total(row *sql.Row) (int, error) {
	type agg struct{ n int }
	var a agg
	err := row.Scan(&a.n)
	return a.n, err
}
