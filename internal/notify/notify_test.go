package notify

import "testing"

func TestReasonLabel(t *testing.T) {
	cases := []struct {
		reason, want string
	}{
		{"mention", "Mentioned"},
		{"team_mention", "Mentioned"},
		{"review_requested", "Review requested"},
		{"assign", "Assigned"},
		{"comment", "New comment"},
		{"author", "GitHub notification"},
		{"subscribed", "GitHub notification"},
		{"", "GitHub notification"},
	}
	for _, c := range cases {
		if got := reasonLabel(c.reason); got != c.want {
			t.Errorf("reasonLabel(%q) = %q, want %q", c.reason, got, c.want)
		}
	}
}

func TestAppleString(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"plain", `"plain"`},
		{`with "quotes"`, `"with \"quotes\""`},
		{`back\slash`, `"back\\slash"`},
		{`mixed "and" \stuff`, `"mixed \"and\" \\stuff"`},
		{"", `""`},
	}
	for _, c := range cases {
		got := appleString(c.in)
		if got != c.want {
			t.Errorf("appleString(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
