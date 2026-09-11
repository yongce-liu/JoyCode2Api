package common

import "testing"

func TestTruncate(t *testing.T) {
	cases := []struct {
		name   string
		s      string
		maxLen int
		want   string
	}{
		{"shorter than limit", "abc", 5, "abc"},
		{"equal to limit", "abcde", 5, "abcde"},
		{"longer than limit", "abcdef", 3, "abc..."},
		{"zero limit", "abc", 0, ""},
		{"negative limit does not panic", "abc", -1, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Truncate(tc.s, tc.maxLen); got != tc.want {
				t.Errorf("Truncate(%q, %d) = %q, want %q", tc.s, tc.maxLen, got, tc.want)
			}
		})
	}
}
