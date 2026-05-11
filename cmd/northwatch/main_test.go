package main

import "testing"

func TestNormalizeAddr(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"", defaultAddr},
		{":8080", ":8080"},
		{"8080", ":8080"},
		{"127.0.0.1:8080", "127.0.0.1:8080"},
		// non-numeric, no colon → pass through so net.Listen surfaces
		// its own error rather than us producing ":localhost".
		{"localhost", "localhost"},
		{"abc123", "abc123"},
	}
	for _, tc := range cases {
		if got := normalizeAddr(tc.in); got != tc.want {
			t.Errorf("normalizeAddr(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
