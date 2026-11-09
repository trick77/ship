package plain

// IB is filled through a builder called on an interface.
type IB struct{ A int }

func (r *IB) dests() []any { return []any{&r.A} }

type destser interface{ dests() []any }

// LB is filled through a closure handed to a helper as its builder.
type LB struct{ A int }

func list[T any](s scanner, cols func(*T) []any) (t T, err error) {
	err = s.Scan(cols(&t)...)
	return
}

// CF is filled through a dest slice a cursor keeps in a field.
type CF struct{ A int }

type cursor struct {
	row  CF
	dest []any
}

// TF is filled through a builder forwarding another's tuple.
type TF struct{ A int }

func (r *TF) base() ([]any, error)  { return []any{&r.A}, nil }
func (r *TF) dests() ([]any, error) { return r.base() }

// Key is handed to a lookup whose own pointer is scanned: not a row type.
type Key struct{ K int64 }

type cache struct{ n int64 }

func (c *cache) slot(k *int64) *int64 { return &c.n }

// FW is filled through a row's Scan forwarded through a local first.
type FW struct{ A int }

func third(s scanner, d destser, c *cache) (ib IB, lb LB, cf CF, tf TF, k Key, fw FW, err error) {
	d = &ib
	if err = s.Scan(d.dests()...); err != nil {
		return
	}
	if lb, err = list(s, func(r *LB) []any { return []any{&r.A} }); err != nil {
		return
	}
	cur := &cursor{}
	cur.dest = []any{&cur.row.A}
	if err = s.Scan(cur.dest...); err != nil {
		return
	}
	cf = cur.row
	td, err := tf.dests()
	if err != nil {
		return
	}
	if err = s.Scan(td...); err != nil {
		return
	}
	p := c.slot(&k.K)
	if err = s.Scan(p); err != nil {
		return
	}
	scan := s.Scan
	err = scanWith2(scan, &fw)
	return
}

func scanWith2(scan func(...any) error, f *FW) error { return scan(&f.A) }
