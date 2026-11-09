package plain

import (
	"fmt"
	"time"
)

func ptr(s *string) *string { return s }

// R2 is a query's argument through a wrapper returning a pointer: its slice reaches no Scan, not a row type.
type R2 struct {
	ID   int64
	Name string
}

func insert2(r R2, exec func(args ...any)) {
	exec([]any{r.ID, ptr(&r.Name)}...)
}

// Cfg is parsed by fmt.Sscan, no row's Scan: not a row type.
type Cfg struct{ Port int }

func parse(line string) (c Cfg, err error) {
	_, err = fmt.Sscan(line, []any{&c.Port}...)
	return
}

// N is filled through a conversion.
type N struct{ N int64 }

// X is filled through a helper forwarding its varargs to Scan.
type X struct{ B int }

// T is filled through a dest builder's pointer parameter.
type T struct{ At time.Time }

// FV is filled through a row's Scan handed on as a func value.
type FV struct{ A int }

func run(s scanner, extra ...any) error {
	d := []any{}
	d = append(d, extra...)
	return s.Scan(d...)
}

func stamps(at *time.Time) []any { return []any{timeAt(at)} }

func scanWith(scan func(...any) error, f *FV) error { return scan(&f.A) }

func flows(s scanner) (n N, x X, t T, u U, fv FV, err error) {
	if err = s.Scan((*int64)(&n.N)); err != nil {
		return
	}
	if err = run(s, &x.B); err != nil {
		return
	}
	if err = s.Scan(stamps(&t.At)...); err != nil {
		return
	}
	if err = s.Scan(u.dests()...); err != nil {
		return
	}
	err = scanWith(s.Scan, &fv)
	return
}
