package plain

type scanner interface{ Scan(dest ...any) error }

// P is filled through a row's Scan(dest ...any) in a file that imports no database: a row type all the same.
type P struct{ F int }

func fill(s scanner) (p P, err error) {
	err = s.Scan(&p.F)
	return
}
