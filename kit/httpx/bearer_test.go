package httpx

import (
	"net/http/httptest"
	"testing"
)

func TestBearer(t *testing.T) {
	for _, tc := range []struct {
		header, token string
		ok            bool
	}{
		{"Bearer abc", "abc", true},
		{"bearer abc", "abc", true},
		{"BEARER   abc  ", "abc", true},
		{"Bearer", "", false},
		{"Bearer ", "", false},
		{"Basic abc", "", false},
		{"Bearerabc", "", false},
		{"", "", false},
	} {
		r := httptest.NewRequest("GET", "/", nil)
		if tc.header != "" {
			r.Header.Set("Authorization", tc.header)
		}
		if token, ok := Bearer(r); token != tc.token || ok != tc.ok {
			t.Errorf("Bearer(%q) = %q, %v; want %q, %v", tc.header, token, ok, tc.token, tc.ok)
		}
	}
}
