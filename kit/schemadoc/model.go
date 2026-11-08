package schemadoc

import (
	"slices"
)

// Schema is the tables of a database, ordered by name.
//
// Table: -
type Schema struct {
	Tables []Table
}

// table is the table named name, nil when there is none.
func (s Schema) table(name string) *Table {
	for i := range s.Tables {
		if s.Tables[i].Name == name {
			return &s.Tables[i]
		}
	}
	return nil
}

// Table is one table. Module names a virtual table's module (vec0, fts5), empty for a plain table; ModuleArgs is
// what its CREATE statement passed the module, on one line. A module may declare its columns untyped (vec0 does), so
// the arguments are where a vector's width shows.
//
// Table: -
type Table struct {
	Name        string
	Module      string
	ModuleArgs  string
	Columns     []Column
	ForeignKeys []ForeignKey
	Indexes     []Index
}

// pkColumns is t's primary key columns in key order.
func (t Table) pkColumns() []string {
	var pk []Column
	for _, c := range t.Columns {
		if c.PK > 0 {
			pk = append(pk, c)
		}
	}
	slices.SortFunc(pk, func(a, b Column) int { return a.PK - b.PK })
	var out []string
	for _, c := range pk {
		out = append(out, c.Name)
	}
	return out
}

// Column is one column. PK is its 1-based position in the primary key, 0 when not part of it. Default is the
// default's SQL text, meaningful only when HasDefault.
//
// Table: -
type Column struct {
	Name       string
	Type       string
	NotNull    bool
	Default    string
	HasDefault bool
	PK         int
}

// ForeignKey is one, possibly composite, foreign key. OnUpdate and OnDelete are spelled as in SQL (NO ACTION,
// CASCADE, SET NULL, SET DEFAULT, RESTRICT).
//
// Table: -
type ForeignKey struct {
	Columns    []string
	RefTable   string
	RefColumns []string
	OnUpdate   string
	OnDelete   string
}

// Index is one secondary index. A primary key's own index is left out; a UNIQUE constraint's is kept. Columns are
// key columns in order, with " DESC" appended where descending; Where is a partial index's predicate.
//
// Table: -
type Index struct {
	Name    string
	Columns []string
	Unique  bool
	Where   string
}
