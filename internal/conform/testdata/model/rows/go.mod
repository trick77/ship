module example.com/rows

go 1.24

require github.com/jackc/pgx/v5 v5.0.0

replace github.com/jackc/pgx/v5 => ./fakepgx
