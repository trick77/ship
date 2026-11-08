package scanx

import "database/sql"

// Scan is a package's func, no method.
func Scan(r *sql.Row, dest ...any) error { return r.Scan(dest...) }
