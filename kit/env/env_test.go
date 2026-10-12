package env

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func lookup(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestValues(t *testing.T) {
	r := New(lookup(map[string]string{
		"S": " v ", "I": "42", "F": "0.5", "B": "YES", "D": "90s",
		"U": "https://example.com/x", "L": "a, ,b,", "BLANK": "  ",
	}))
	if got := r.String("S", "d"); got != "v" {
		t.Errorf("String = %q", got)
	}
	if got := r.String("BLANK", "d"); got != "d" {
		t.Errorf("blank String = %q, want default", got)
	}
	if got := r.Int("I", 1); got != 42 {
		t.Errorf("Int = %d", got)
	}
	if got := r.Int("NONE", 7); got != 7 {
		t.Errorf("unset Int = %d", got)
	}
	if got := r.Float("F", 1); got != 0.5 {
		t.Errorf("Float = %v", got)
	}
	if got := r.Float("NONE", 2); got != 2 {
		t.Errorf("unset Float = %v", got)
	}
	if got := r.Bool("B", false); !got {
		t.Error("Bool YES = false")
	}
	if got := r.Bool("NONE", true); !got {
		t.Error("unset Bool lost its default")
	}
	if got := r.Duration("D", 0); got != 90*time.Second {
		t.Errorf("Duration = %v", got)
	}
	if got := r.Duration("NONE", time.Minute); got != time.Minute {
		t.Errorf("unset Duration = %v", got)
	}
	if got := r.URL("U", ""); got == nil || got.Host != "example.com" {
		t.Errorf("URL = %v", got)
	}
	if got := r.URL("NONE", ""); got != nil {
		t.Errorf("unset URL = %v, want nil", got)
	}
	if got := r.URL("NONE", "http://d.example"); got == nil || got.Host != "d.example" {
		t.Errorf("default URL = %v", got)
	}
	if got := r.List("L", nil); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("List = %q", got)
	}
	if got := r.List("NONE", []string{"x"}); !reflect.DeepEqual(got, []string{"x"}) {
		t.Errorf("unset List = %q", got)
	}
	if got := r.Bool("NONE2", false); got {
		t.Error("unset Bool = true")
	}
	if err := r.Err(); err != nil {
		t.Fatalf("Err = %v", err)
	}
}

func TestBoolSpellings(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want bool
	}{{"true", true}, {"1", true}, {"On", true}, {"false", false}, {"0", false}, {"NO", false}, {"off", false}} {
		r := New(lookup(map[string]string{"B": tt.in}))
		if got := r.Bool("B", !tt.want); got != tt.want || r.Err() != nil {
			t.Errorf("Bool(%q) = %v, err %v", tt.in, got, r.Err())
		}
	}
}

// Every fault is reported, in call order, never just the first.
func TestFaultsInOrder(t *testing.T) {
	r := New(lookup(map[string]string{
		"I": "4x", "F": "NaN", "B": "ture", "D": "30", "U": "/relative", "U2": "ftp://h",
	}))
	r.Required("MISSING")
	if got := r.Int("I", 3); got != 3 {
		t.Errorf("bad Int returned %d, want the default", got)
	}
	r.Float("F", 0)
	r.Bool("B", false)
	r.Duration("D", 0)
	r.URL("U", "")
	r.URL("U2", "")
	r.Fail("%s must not exceed %s", "A", "B")
	err := r.Err()
	if err == nil {
		t.Fatal("Err = nil")
	}
	want := []string{"MISSING is required", `I: "4x"`, `F: "NaN"`, `B: "ture"`, `D: "30"`, `U: "/relative"`,
		`U2: "ftp://h"`, "A must not exceed B"}
	lines := strings.Split(err.Error(), "\n")
	if len(lines) != len(want) {
		t.Fatalf("got %d faults, want %d:\n%v", len(lines), len(want), err)
	}
	for i, w := range want {
		if !strings.HasPrefix(lines[i], w) {
			t.Errorf("fault %d = %q, want prefix %q", i, lines[i], w)
		}
	}
}
