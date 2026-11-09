// Package pgx stands in for pgx v5: ForEachRow scans its scans slice itself.
package pgx

// Rows is a result's rows.
type Rows interface{ Next() bool }

// ForEachRow scans each row into scans, then calls fn.
func ForEachRow(rows Rows, scans []any, fn func() error) error { return nil }
