package plain

import "time"

type stamp struct{ t *time.Time }

func (s stamp) Scan(v any) error { return nil }

func timeAt(t *time.Time) stamp { return stamp{t} }

func boxed(s stamp) any { return s }

// W is filled only through a wrapper in a wrapper.
type W struct{ At time.Time }

// V is filled only through a wrapper returning a scanner, in a dest slice.
type V struct{ At time.Time }

func wrapped(s scanner) (w W, v V, err error) {
	if err = s.Scan(boxed(timeAt(&w.At))); err != nil {
		return
	}
	dest := []any{timeAt(&v.At)}
	err = s.Scan(dest...)
	return
}

// J only holds a W: the dest builder's own literal credits W, not J.
type J struct{ W W }

func wDest(w *W) []any { return []any{timeAt(&w.At)} }

func joined(s scanner) (j J, err error) {
	err = s.Scan(append(wDest(&j.W), boxed(stamp{}))...)
	return
}

// O is filled by a sql.Scanner-like Scan(any) next to a row's Scan: not a row type.
type O struct{ G int }

func scanned() (o O, err error) { return o, one{}.Scan(&o.G) }

type null struct{ s *string }

func nullable(s *string) null { return null{s} }

// R is a query's argument through a wrapper returning no scanner: not a row type.
type R struct{ Name string }

func insert(r R, exec func(args ...any)) {
	args := []any{nullable(&r.Name)}
	exec(args...)
}
