package store

import "database/sql"

// Inner declares the field a nested selector fills: credited, Outer not.
type Inner struct{ F int }

// Base is embedded: its promoted field credits it.
type Base struct{ ID int64 }

// Outer only holds them.
type Outer struct {
	Base
	In Inner
}

func outer(row *sql.Row) (Outer, error) {
	var o Outer
	err := row.Scan(&o.In.F, &o.ID)
	return o, err
}

// Anon's unnamed struct field credits Anon.
type Anon struct {
	Meta struct{ Name string }
}

func anon(row *sql.Row) (a Anon, err error) {
	err = row.Scan(&a.Meta.Name)
	return
}
