package broken

import "database/sql"

var _ sql.Row = undefined
