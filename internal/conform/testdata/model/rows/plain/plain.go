package plain

type scanner interface{ Scan(dest ...any) error }

// P is filled by a Scan in a file that imports no database: not a row type.
type P struct{ F int }

func fill(s scanner) (p P, err error) {
	err = s.Scan(&p.F)
	return
}
