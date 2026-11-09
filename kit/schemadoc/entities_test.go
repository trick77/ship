package schemadoc

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestEntities(t *testing.T) {
	got, err := Entities("testdata/module")
	if err != nil {
		t.Fatal(err)
	}
	want := []Entity{
		{Package: "channels", Dir: "channels", Name: "Channel", Table: "channels",
			Doc: "Channel is one row of the channels table.\nSubscribers is 0 when the count is hidden.\n\nThe second paragraph is not part of the summary.",
			Fields: []Field{
				{Name: "ID", Type: "string"},
				{Name: "Name", Type: "string", Doc: "Name is the display name. A second doc line is kept."},
				{Name: "Subscribers", Type: "int64", Doc: "trailing comment, used when there is no doc"},
				{Name: "AddedAt", Type: "*time.Time"},
				{Name: "Tags", Type: "[]string"},
				{Name: "Extra", Type: "map[string]any"},
				{Name: "private", Type: "string"},
				{Name: "Duration", Type: "time.Duration", Embedded: true},
			}},
		{Package: "channels", Dir: "channels", Name: "Stats", Table: "-",
			Doc: "Stats is computed per request, never stored.", Fields: []Field{
				{Name: "Count", Type: "int", Doc: "Count is the number of videos, hidden ones included."},
				{Name: "Since", Type: "string", Doc: "trailing comment"},
				{Name: "From", Type: "string", Doc: "From and To bound the count.\n\nBoth are UTC."},
				{Name: "To", Type: "string", SharedDoc: "From"}, // under From's comment
				{Name: "Until", Type: "string"},                 // a blank line ends the run
				{Name: "Hits", Type: "int", Doc: "Hits is the cache hits."},
				{Name: "Misses", Type: "int"}, // Hits' doc opens with Hits: about it alone
				{Name: "Odd", Type: "int"},    // a directive is no doc
				{Name: "Even", Type: "int"},   // nor a shared one
				{Name: "Lo", Type: "int", Doc: "The window's edges."},
				{Name: "Hi", Type: "int", SharedDoc: "Lo"}, // one line under one comment
				{Name: "Req", Type: "int", Doc: "The request id."},
				{Name: "Resp", Type: "int"}, // not named by Req's comment
			}},
		{Package: "channels", Dir: "channels", Name: "Untagged", Doc: "Untagged has no Table line."},
		{Package: "channels", Dir: "channels", Name: "Video", Table: "videos",
			Doc: "Video is one row of videos, declared in a group.",
			Fields: []Field{
				{Name: "ID", Type: "string"}, {Name: "ChannelID", Type: "string"},
				{Name: "A", Type: "int"}, {Name: "B", Type: "int"},
			}},
		{Package: "channels", Dir: "channels", Name: "legacyRow", Table: "channels",
			Doc: "legacyRow is unexported and still listed, with its unexported fields; a blank field is not.",
			Fields: []Field{
				{Name: "id", Type: "string"},
				{Name: "row", Type: "*row", Doc: "embedded, unexported", Embedded: true},
			}},
		{Package: "tags", Dir: "videos/tags", Name: "Tag", Table: "tags", Doc: "Tag is one row of tags.",
			Fields: []Field{{Name: "VideoID", Type: "string"}, {Name: "Tag", Type: "string"}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("entities:\n got %+v\nwant %+v", got, want)
	}
	if s := got[0].Summary(); s != "Channel is one row of the channels table. Subscribers is 0 when the count is hidden." {
		t.Errorf("summary = %q", s)
	}
}

func TestEntitiesErrors(t *testing.T) {
	if _, err := Entities(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("missing dir: no error")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "model.go"), []byte("package broken\n\ntype X struct {"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Entities(dir); err == nil || !strings.Contains(err.Error(), "model.go") {
		t.Errorf("syntax error: err = %v, want it to name the file", err)
	}
}
