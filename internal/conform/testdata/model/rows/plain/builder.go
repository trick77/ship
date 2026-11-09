package plain

// U's dest builder sits in a file holding no Scan: its slice reaches one elsewhere, a row type all the same.
type U struct{ A int }

func (u *U) dests() []any { return []any{&u.A} }
