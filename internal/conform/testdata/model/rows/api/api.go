package api

import (
	"database/sql"

	"example.com/rows/domain"
)

// sql.NullString is the standard library's: never this module's row type.
func user(row *sql.Row) (domain.User, error) {
	var u domain.User
	var ns sql.NullString
	err := row.Scan(&u.Name, &ns.String)
	return u, err
}
