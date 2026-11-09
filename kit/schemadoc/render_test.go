package schemadoc

import (
	"bytes"
	"slices"
	"strings"
	"testing"
)

func fixtureEntities(t *testing.T) []Entity {
	t.Helper()
	es, err := Entities("testdata/module")
	if err != nil {
		t.Fatal(err)
	}
	return slices.DeleteFunc(es, func(e Entity) bool { return e.Table == "" })
}

func TestRenderGolden(t *testing.T) {
	out, err := Render(describeFixture(t), fixtureEntities(t))
	if err != nil {
		t.Fatal(err)
	}
	Golden(t, out, "testdata/fixture.md")
}

func TestRenderIsDeterministic(t *testing.T) {
	s := describeFixture(t)
	es := fixtureEntities(t)
	s.Views = append(s.Views, View{Name: "a_first", Kind: "materialized view"})
	first, err := Render(s, es)
	if err != nil {
		t.Fatal(err)
	}
	slices.Reverse(s.Tables)
	slices.Reverse(s.Views)
	slices.Reverse(es)
	second, err := Render(s, es)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Error("output depends on input order")
	}
}

func TestRenderUnknownTable(t *testing.T) {
	es := append(fixtureEntities(t),
		Entity{Package: "ghosts", Name: "Ghost", Table: "ghosts"},
		Entity{Package: "ghosts", Name: "Spirit", Table: "spirits"})
	_, err := Render(describeFixture(t), es)
	if err == nil || !strings.Contains(err.Error(), "ghosts.Ghost: no table or view ghosts") || !strings.Contains(err.Error(), "ghosts.Spirit: no table or view spirits") {
		t.Errorf("err = %v, want both unknown tables named", err)
	}
}

func TestRenderMissingTableLine(t *testing.T) {
	es, err := Entities("testdata/module")
	if err != nil {
		t.Fatal(err)
	}
	_, err = Render(describeFixture(t), es)
	if err == nil || !strings.Contains(err.Error(), "channels.Untagged: no Table: line") {
		t.Errorf("err = %v, want the untagged type named", err)
	}
}

func TestMermaidIdent(t *testing.T) {
	for in, want := range map[string]string{
		"TEXT": "TEXT", "float[768]": "float_768_", "varchar(255)": "varchar_255_",
		"timestamp with time zone": "timestamp_with_time_zone", "": "ANY", "2col": "_2col", "my-col": "my_col",
	} {
		if got := mermaidIdent(in); got != want {
			t.Errorf("mermaidIdent(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMermaidType(t *testing.T) {
	for in, want := range map[string]string{
		"TEXT": "TEXT", "text[]": "text[]", "float[768]": "float[768]", "vector(768)": "vector(768)",
		"timestamp with time zone": "timestamp_with_time_zone", "numeric(10,2)": "numeric(10_2)",
		"character varying(255)": "character_varying(255)", "x-y": "x-y", "my.type": "my_type", `"q"`: `_q_`,
		"": "ANY", "2col": "_2col", "[x]": "_[x]",
	} {
		if got := mermaidType(in); got != want {
			t.Errorf("mermaidType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRenderChecksAndViews(t *testing.T) {
	s := Schema{
		Tables: []Table{{Name: "t", Columns: []Column{{Name: "x", Type: "INTEGER"}}, Checks: []string{"CHECK (x > 0)"}}},
		Views:  []View{{Name: "v", Kind: "view"}},
	}
	out, err := Render(s, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"\nChecks:\n\n- `CHECK (x > 0)`\n", "\n## Views\n\n- `v` (view)\n"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

// A child keyed by its parent's id (its PK) or by a unique key has at most one row per parent; a partial unique
// index or a plain one does not make it so.
func TestRenderMermaidOneToOne(t *testing.T) {
	parent := Table{Name: "p", Columns: []Column{{Name: "id", Type: "INTEGER", NotNull: true, PK: 1}}}
	child := func(name string, idx ...Index) Table {
		return Table{Name: name, Columns: []Column{{Name: "id", Type: "INTEGER", NotNull: true, PK: 1},
			{Name: "p_id", Type: "INTEGER", NotNull: true}},
			ForeignKeys: []ForeignKey{{Columns: []string{"p_id"}, RefTable: "p", RefColumns: []string{"id"}}}, Indexes: idx}
	}
	byPK := Table{Name: "pk", Columns: []Column{{Name: "p_id", Type: "INTEGER", NotNull: true, PK: 1}},
		ForeignKeys: []ForeignKey{{Columns: []string{"p_id"}, RefTable: "p", RefColumns: []string{"id"}}}}
	// A composite key holding a unique one is one-to-one too.
	wide := child("wide", Index{Name: "u", Columns: []string{"p_id"}, Keys: []string{"p_id"}, Unique: true})
	wide.Columns = append(wide.Columns, Column{Name: "tenant", Type: "TEXT", NotNull: true})
	wide.ForeignKeys[0].Columns = []string{"p_id", "tenant"}
	s := Schema{Tables: []Table{parent, byPK, wide,
		// Keys, not the displayed Columns, decide.
		child("uniq", Index{Name: "u", Columns: []string{`"p_id" COLLATE NOCASE DESC`}, Keys: []string{"p_id"}, Unique: true}),
		child("expr", Index{Name: "u", Columns: []string{"lower(p_id)"}, Keys: []string{""}, Unique: true}),
		child("partial", Index{Name: "u", Columns: []string{"p_id"}, Keys: []string{"p_id"}, Unique: true, Where: "p_id > 0"}),
		// A predicate keeping only NULL keys out leaves the index unique.
		child("guarded", Index{Name: "u", Columns: []string{"p_id"}, Keys: []string{"p_id"}, Unique: true,
			Where: "(p_id IS NOT NULL)"}),
		child("plain", Index{Name: "i", Columns: []string{"p_id"}, Keys: []string{"p_id"}})}}
	out, err := Render(s, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"pk |o--|| p", "uniq |o--|| p", "wide |o--|| p", "expr }o--|| p",
		"partial }o--|| p", "guarded |o--|| p", "plain }o--|| p"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

// The intro counts views per kind, known kinds first in a fixed order.
func TestRenderViewCounts(t *testing.T) {
	cagg := func(name string) View { return View{Name: name, Kind: "continuous aggregate"} }
	for _, tc := range []struct {
		s    Schema
		want string
	}{
		{Schema{Tables: []Table{{Name: "a"}, {Name: "b"}}}, "2 tables;"},
		{Schema{Tables: []Table{{Name: "a"}}, Views: []View{cagg("c1"), cagg("c2")}}, "1 table, 2 continuous aggregates;"},
		{Schema{Views: []View{cagg("c"), {Name: "z", Kind: "zz"}, {Name: "m", Kind: "materialized view"},
			{Name: "v1", Kind: "view"}, {Name: "v2"}, {Name: "y", Kind: "yy"}}},
			"0 tables, 2 views, 1 materialized view, 1 continuous aggregate, 1 yy, 1 zz;"},
	} {
		out, err := Render(tc.s, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(out), "\n"+tc.want) {
			t.Errorf("output lacks %q:\n%s", tc.want, out)
		}
	}
}

func TestNullGuard(t *testing.T) {
	keys := []string{"a", "b"}
	for where, want := range map[string]bool{
		"":                                      true,
		"a IS NOT NULL":                         true,
		"(a IS NOT NULL)":                       true,
		"A is not null":                         true,
		`"a" IS NOT NULL`:                       true,
		"[b] NOTNULL":                           true,
		"`b` NOT NULL":                          true,
		`"A" IS NOT NULL`:                       false, // a quoted name keeps its case in Postgres
		"'a' IS NOT NULL":                       false,
		`"a AND b" IS NOT NULL`:                 false,
		`a IS NOT NULL AND "a(" IS NOT NULL`:    false,
		`"a IS NOT NULL`:                        false,
		"a IS NOT NULL AND b IS NOT NULL":       true,
		"((a IS NOT NULL) AND (b IS NOT NULL))": true,
		"a IS NULL":                             false,
		"c IS NOT NULL":                         false,
		"a IS NOT NULL OR b IS NOT NULL":        false,
		"a IS NOT NULL AND state <> 'done'":     false,
		"state <> 'done'":                       false,
		"lower(a) IS NOT NULL":                  false,
		"NOT (a IS NOT NULL)":                   false,
		"()":                                    false,
	} {
		if got := nullGuard(where, keys); got != want {
			t.Errorf("nullGuard(%q) = %v, want %v", where, got, want)
		}
	}
}

// A field under another's comment points to it; a field doc's paragraphs keep their break.
func TestRenderFieldDocs(t *testing.T) {
	e := Entity{Package: "store", Dir: "store", Name: "Clip", Table: NoTable, Fields: []Field{
		{Name: "Prompt", Type: "string", Doc: "The clip's parts.\n\nEach may be empty."},
		{Name: "M", Type: "map[string]int", Doc: "A map<string>int & co, `a<b>`."},
		{Name: "Review", Type: "string", SharedDoc: "Prompt"},
	}}
	out, err := Render(Schema{Views: []View{{Name: "v", Kind: "view"}}}, []Entity{e})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"| `Prompt` | `string` | The clip's parts.<br>Each may be empty. |",
		"| `Review` | `string` | see `Prompt` |", "0 tables, 1 view;",
		"| A map&lt;string&gt;int &amp; co, `a<b>`. |"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestRenderViewRow(t *testing.T) {
	s := Schema{Views: []View{{Name: "active", Kind: "view"}}}
	out, err := Render(s, []Entity{{Package: "store", Dir: "store", Name: "Active", Table: "active"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := "\n## Views\n\n- `active` (view)\n  - `store.Active` (store)\n"; !strings.Contains(string(out), want) {
		t.Errorf("output lacks %q:\n%s", want, out)
	}
}
