package records

import "testing"

func TestLiteralTextSearchIncludesMultilineMessagesAndAttributes(t *testing.T) {
	r := Record{Message: "Error\n  at worker", Attributes: map[string]string{"reason": "connection refused"}}
	for _, test := range []struct {
		text string
		want bool
	}{
		{"", true}, {"Error\n  at", true}, {"connection", true}, {"reason", true}, {"error", false}, {".*", false},
	} {
		if got := (Filter{Text: test.text}).Matches(r); got != test.want {
			t.Fatal(test.text, got)
		}
	}
}
