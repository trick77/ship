package plain

import (
	"fmt"

	"github.com/jackc/pgx/v5"
)

// TB is filled through a builder's tuple result.
type TB struct{ A int }

func (r *TB) dests() ([]any, error) { return []any{&r.A}, nil }

// Ans is read by fmt.Scan, a package's Scan that reads no row: not a row type.
type Ans struct{ Answer string }

// CL is filled through a closure forwarding its varargs to Scan.
type CL struct{ A int }

// ME is filled through a helper called as a method expression.
type ME struct{ A int }

type helper struct{}

func (h *helper) run(s scanner, extra ...any) error { return s.Scan(extra...) }

// G is filled through an instantiated generic builder.
type G struct{ A int }

func destsOf[T any](g *G) []any { return []any{&g.A} }

// FE is filled by pgx's ForEachRow through its scans slice.
type FE struct{ A int }

func more(s scanner, rows pgx.Rows, h *helper) (tb TB, a Ans, cl CL, me ME, g G, fe FE, err error) {
	d, err := tb.dests()
	if err != nil {
		return
	}
	if err = s.Scan(d...); err != nil {
		return
	}
	if _, err = fmt.Scan(&a.Answer); err != nil {
		return
	}
	scan := func(dest ...any) error { return s.Scan(dest...) }
	if err = scan(&cl.A); err != nil {
		return
	}
	if err = (*helper).run(h, s, &me.A); err != nil {
		return
	}
	if err = s.Scan(destsOf[int](&g)...); err != nil {
		return
	}
	err = pgx.ForEachRow(rows, []any{&fe.A}, func() error { return nil })
	return
}
