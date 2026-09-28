package schema

import (
	"testing"
)

func TestQuoteIdent(t *testing.T) {
	cases := map[string]string{
		"invoices":     "invoices",
		"due_date2":    "due_date2",
		"Display Name": `"Display Name"`,
		"user":         `"user"`,
		"order":        `"order"`,
		"2fa":          `"2fa"`,
		"a$b":          `"a$b"`,
		`say "hi"`:     `"say ""hi"""`,
		"bıgınt":       `"bıgınt"`,
	}
	for in, want := range cases {
		if got := QuoteIdent(in); got != want {
			t.Errorf("QuoteIdent(%q) = %s, want %s", in, got, want)
		}
	}
}
