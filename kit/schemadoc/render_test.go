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
