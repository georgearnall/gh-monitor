package notify

import "testing"

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
