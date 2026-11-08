package plain

// lines is bufio.Scanner-like: its Scan() reads no row.
type lines struct{}

func (lines) Scan() bool { return false }

// one is sql.Scanner-like: its Scan(any) fills its receiver, no row.
type one struct{}

func (one) Scan(v any) error { return nil }

// Q is filled in a file without a row's Scan: not a row type.
type Q struct{ F, G int }

func read(l lines, o one) (q Q, err error) {
	for l.Scan() {
	}
	dest := []any{&q.F}
	_ = dest
	return q, o.Scan(&q.G)
}
