package client

import "testing"

func TestOlderThan(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"v0.9.0", "v0.10.0", true},
		{"v0.10.0", "v0.9.0", false},
		{"v1.2.3", "v1.2.3", false},
		{"v1.2.3-rc.1", "v1.2.4", true},
		{"dev", "v1.0.0", false},
		{"v1.0.0", "", false},
	}
	for _, c := range cases {
		if got := OlderThan(c.a, c.b); got != c.want {
			t.Errorf("OlderThan(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
