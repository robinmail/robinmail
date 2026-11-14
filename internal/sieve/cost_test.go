package sieve

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// What one run may cost is bounded as a whole, not only per command: a
// script is the user's, the message is a stranger's, and a test compares
// every value with every key. These are the most expensive scripts that
// fit in MaxScriptSize, against messages made for them. Without the bound
// the first ran for more than a minute.
func TestRunCostIsBounded(t *testing.T) {
	fill := func(test string) string {
		var b strings.Builder
		b.WriteString("require [\"body\"];\n")
		for b.Len() < MaxScriptSize-len(test)-100 {
			b.WriteString("if " + test + " { discard; }\n")
		}
		return b.String()
	}
	list := func(item string, n int) string {
		return "[" + strings.TrimSuffix(strings.Repeat(item+",", n), ",") + "]"
	}
	var manyFields strings.Builder
	for i := 0; i < 20000; i++ {
		fmt.Fprintf(&manyFields, "X-%d: w\r\n", i)
	}
	for _, c := range []struct{ name, script, message string }{
		{"matches", fill(`header :matches "subject" ` + list(`"*a*a*a*a*a*a*a*a*b"`, 40)),
			strings.Repeat("Subject: "+strings.Repeat("a", 900)+"\r\n", 5) + "\r\nbody\r\n"},
		{"names", fill(`header :is ` + list(`"x"`, 500) + ` "v"`), manyFields.String() + "\r\nbody\r\n"},
		{"body", fill(`body :raw :contains ` + list(`"zzzzzzzz"`, 20)),
			"Subject: s\r\n\r\n" + strings.Repeat(strings.Repeat("y", 70)+"\r\n", 12000)},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, err := Parse([]byte(c.script))
			if err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			res := s.Run(Env{Raw: []byte(c.message), Now: start})
			took := time.Since(start)
			t.Logf("%v, err: %v", took, res.Err)
			// A stopped run keeps the message (RFC 5228 2.10.6).
			if res.Err != nil && (len(res.Actions) != 1 || res.Actions[0].Kind != Keep) {
				t.Errorf("actions %+v, want keep alone", res.Actions)
			}
			// Well under a second here; the bound is what is tested.
			if took > 10*time.Second {
				t.Errorf("the run took %v", took)
			}
		})
	}
}

// A script within reason is nowhere near the bound.
func TestRunCostOfAnOrdinaryScript(t *testing.T) {
	var b strings.Builder
	b.WriteString("require [\"fileinto\", \"body\"];\n")
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&b, "if anyof (header :contains [\"subject\", \"from\", \"to\"] [\"word%d\", \"other%d\"], header :matches \"list-id\" \"*list%d*\") { fileinto \"L%d\"; }\n", i, i, i, i)
	}
	b.WriteString("if body :text :contains [\"unsubscribe\", \"newsletter\"] { fileinto \"News\"; }\n")
	s, err := Parse([]byte(b.String()))
	if err != nil {
		t.Fatal(err)
	}
	msg := "From: a@example.test\r\nTo: b@example.test\r\nSubject: hello there\r\nList-Id: <x.example.test>\r\n" +
		"Content-Type: text/plain\r\n\r\n" + strings.Repeat("some text of a long message\r\n", 30000)
	res := s.Run(Env{Raw: []byte(msg), Now: time.Now()})
	if res.Err != nil {
		t.Fatalf("an ordinary script failed: %v", res.Err)
	}
}
