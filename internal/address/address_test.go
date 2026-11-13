package address

import "testing"

func TestParse(t *testing.T) {
	valid := map[string]string{
		"mike@example.test":          "mike@example.test",
		" Mike.Page@Example.TEST":    "mike.page@example.test",
		"a+tag_1-x@sub.example.test": "a+tag_1-x@sub.example.test",
		"root@localhost":             "root@localhost",
	}
	for in, want := range valid {
		a, err := Parse(in)
		if err != nil {
			t.Errorf("Parse(%q): %v", in, err)
			continue
		}
		if a.String() != want {
			t.Errorf("Parse(%q) = %q, want %q", in, a, want)
		}
	}

	invalid := []string{
		"", "@", "mike", "mike@", "@example.test",
		"../etc@example.test", "a/b@example.test", "a@b/c",
		".mike@example.test", "mike.@example.test", "mi..ke@example.test",
		"mike@.example.test", "mike@example..test", "mike@-example.test",
		"mike@example.test.", "mi ke@example.test", "mike@exa mple.test",
		"mike\x00@example.test", "müller@example.test",
	}
	for _, in := range invalid {
		if a, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) = %q, want error", in, a)
		}
	}
}
