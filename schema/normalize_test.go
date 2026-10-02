package schema

import (
	"testing"
)

func TestCutLast(t *testing.T) {
	cases := []struct{ in, before, after string }{
		{"public.invoices.due_date", "public.invoices", "due_date"},
		{`public."a.b".c`, `public."a.b"`, "c"},
		{`public.t."x.y"`, "public.t", `"x.y"`},
		{"solo", "", "solo"},
	}
	for _, c := range cases {
		before, after := cutLast(c.in)
		if before != c.before || after != c.after {
			t.Errorf("cutLast(%q) = %q, %q; want %q, %q", c.in, before, after, c.before, c.after)
		}
	}
}
