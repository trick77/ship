package release

import (
	"slices"
	"testing"
)

func TestLatest(t *testing.T) {
	if v := Latest([]string{"1.2.9", "1.10.0", "v9.9.9", "2.0.0-rc1", "", " 1.10.1 "}, ""); v != (Version{1, 10, 1}) {
		t.Fatal(v)
	}
	if v := Latest(nil, ""); v != (Version{}) {
		t.Fatal(v)
	}
	if v := Latest([]string{"0.9.0", "1.0.0", "0.10.0"}, ""); v != (Version{1, 0, 0}) {
		t.Fatal(v)
	}
	if v := Latest([]string{"9.9.9", "v0.2.0", "v0.10.1", "vx"}, "v"); v != (Version{0, 10, 1}) {
		t.Fatal(v)
	}
}

func TestNext(t *testing.T) {
	v := Version{1, 2, 3}
	for bump, want := range map[string]Version{"patch": {1, 2, 4}, "minor": {1, 3, 0}, "major": {2, 0, 0}} {
		if got, err := Next(v, bump); err != nil || got != want {
			t.Errorf("%s: %v %v", bump, got, err)
		}
	}
	if _, err := Next(v, "huge"); err == nil {
		t.Error("bad bump accepted")
	}
}

func TestTags(t *testing.T) {
	if got := Fixed("ghcr.io/o/r", Version{1, 2, 3}, "abc1234"); !slices.Equal(got, []string{"ghcr.io/o/r:1.2.3", "ghcr.io/o/r:abc1234"}) {
		t.Fatal(got)
	}
	if got := Moving("ghcr.io/o/r", Version{1, 2, 3}); !slices.Equal(got, []string{"ghcr.io/o/r:1.2", "ghcr.io/o/r:1", "ghcr.io/o/r:latest"}) {
		t.Fatal(got)
	}
}
