package secret

import "testing"

func TestMatch(t *testing.T) {
	for _, tc := range []struct {
		got, want string
		match     bool
	}{
		{"s3cret", "s3cret", true},
		{"s3cret", "s3cre", false},
		{"s3cre", "s3cret", false},
		{"S3cret", "s3cret", false},
		{"", "s3cret", false},
		{"", "", false},
		{"x", "", false},
	} {
		if got := Match(tc.got, tc.want); got != tc.match {
			t.Errorf("Match(%q, %q) = %v, want %v", tc.got, tc.want, got, tc.match)
		}
	}
}
