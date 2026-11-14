package sieve

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// The tests in this package are written from the RFCs: each names the
// section it checks and uses the RFC's own examples where it has them.

const sample = "From: \"Wile E. Coyote\" <coyote@desert.example.org>\r\n" +
	"To: roadrunner@acme.example.com, Tim <tim@example.com>\r\n" +
	"Cc: me@example.com\r\n" +
	"Subject: =?utf-8?q?$$$_Make_money_fast_$$$?=\r\n" +
	"Date: Tue, 1 Apr 1997 09:06:31 -0800 (PST)\r\n" +
	"Received: from mx.example.org by mail.example.com; Sat, 14 Nov 2026 10:00:00 +0100\r\n" +
	"Received: from origin.example.org by mx.example.org; Sun, 15 Nov 2026 09:59:00 +0100\r\n" +
	"X-Caffeine: C8H10N4O2\r\n" +
	"X-Priority: 2\r\n" +
	"\r\n" +
	"This is the body. The project schedule has slipped.\r\n"

func mustParse(t *testing.T, src string) *Script {
	t.Helper()
	s, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v\n%s", err, src)
	}
	return s
}

// describe writes the actions of a result as one line.
func describe(r Result) string {
	var parts []string
	for _, a := range r.Actions {
		flags := ""
		if len(a.Flags) > 0 {
			flags = " flags=" + strings.Join(a.Flags, ",")
		}
		switch a.Kind {
		case Keep:
			parts = append(parts, "keep"+flags)
		case FileInto:
			create := ""
			if a.Create {
				create = " create"
			}
			parts = append(parts, fmt.Sprintf("fileinto %q%s%s", a.Mailbox, create, flags))
		case Redirect:
			parts = append(parts, "redirect "+a.Address)
		case Discard:
			parts = append(parts, "discard")
		case Reject:
			kind := "reject"
			if a.EReject {
				kind = "ereject"
			}
			parts = append(parts, fmt.Sprintf("%s %q", kind, a.Reason))
		case Vacation:
			v := a.Vacation
			parts = append(parts, fmt.Sprintf("vacation days=%d subject=%q from=%q addresses=%v mime=%v handle=%q %q",
				v.Days, v.Subject, v.From, v.Addresses, v.MIME, v.Handle, v.Reason))
		}
	}
	out := strings.Join(parts, " | ")
	if r.Err != nil {
		out += " ERROR"
	}
	return out
}

var testNow = time.Date(2026, 11, 14, 15, 4, 5, 0, time.FixedZone("", 3600))

func runOn(t *testing.T, script, msg string) string {
	t.Helper()
	return describe(mustParse(t, script).Run(Env{Raw: []byte(msg), From: "coyote@desert.example.org",
		To: "me+sieve@example.com", Now: testNow, MailboxExists: func(name string) bool { return name == "Partners" }}))
}

func expect(t *testing.T, what, script, msg, want string) {
	t.Helper()
	if got := runOn(t, script, msg); got != want {
		t.Errorf("%s:\n got  %s\n want %s\nscript: %s", what, got, want, script)
	}
}

// truth runs "if <test> { discard; }" and reports whether the test held.
func truth(t *testing.T, require, test string) bool {
	t.Helper()
	script := "if " + test + " { discard; }"
	if require != "" {
		script = "require [" + require + "];\n" + script
	}
	return runOn(t, script, sample) == "discard"
}

// RFC 5228 2.10.2: "if a message is not explicitly discarded or delivered
// elsewhere, the implicit keep is taken". 2.10.3: duplicates.
func TestRFC5228_2_10_ImplicitKeep(t *testing.T) {
	expect(t, "an empty script", ``, sample, `keep`)
	expect(t, "discard cancels the implicit keep", `discard;`, sample, `discard`)
	expect(t, "redirect cancels it", `redirect "bart@example.com";`, sample, `redirect bart@example.com`)
	expect(t, "fileinto cancels it", `require "fileinto"; fileinto "x";`, sample, `fileinto "x"`)
	expect(t, "keep is explicit", `keep; discard;`, sample, `keep | discard`)
	// "an implementation MUST NOT file the message into the same mailbox twice"
	expect(t, "the same mailbox twice", `require "fileinto"; fileinto "x"; fileinto "x"; redirect "a@b.example"; redirect "A@B.example";`,
		sample, `fileinto "x" | redirect a@b.example`)
	// 3.3: "the stop action ends all processing"; the implicit keep is taken.
	expect(t, "stop", `stop; discard;`, sample, `keep`)
	expect(t, "stop inside a block", `if true { stop; } discard;`, sample, `keep`)
}

// RFC 5228 3.1: the examples of if, elsif and else.
func TestRFC5228_3_1_Control(t *testing.T) {
	script := `require "fileinto";
if header :contains "from" "coyote" {
   discard;
} elsif header :contains ["subject"] ["$$$"] {
   discard;
} else {
   fileinto "INBOX";
}`
	expect(t, "from coyote", script, sample, `discard`)
	expect(t, "subject with $$$", script, "From: x@y.example\r\nSubject: $$$ now\r\n\r\n", `discard`)
	expect(t, "neither", script, "From: x@y.example\r\nSubject: hello\r\n\r\n", `fileinto "INBOX"`)
	script = `if header :contains ["From"] ["coyote"] {
   redirect "acm@example.com";
} elsif header :contains "Subject" "$$$" {
   redirect "postmaster@example.com";
} else {
   redirect "field@example.com";
}`
	expect(t, "redirects, first", script, sample, `redirect acm@example.com`)
	expect(t, "redirects, second", script, "Subject: $$$\r\n\r\n", `redirect postmaster@example.com`)
	expect(t, "redirects, third", script, "Subject: x\r\n\r\n", `redirect field@example.com`)
	// Only the first branch that holds runs, and an else after it does not.
	expect(t, "one branch", `if true { redirect "a@b.example"; } elsif true { redirect "c@d.example"; } else { discard; }`, sample, `redirect a@b.example`)
	expect(t, "nested", `if true { if false { discard; } else { redirect "a@b.example"; } }`, sample, `redirect a@b.example`)
}

// RFC 5228 4: the examples of the actions.
func TestRFC5228_4_Actions(t *testing.T) {
	expect(t, "4.1 fileinto", `require "fileinto";
if header :contains ["from"] "coyote" {
   fileinto "INBOX.harassment";
}`, sample, `fileinto "INBOX.harassment"`)
	expect(t, "4.2 redirect", `redirect "bart@example.com";`, sample, `redirect bart@example.com`)
	expect(t, "4.3 keep", `if size :under 1M { keep; } else { discard; }`, sample, `keep`)
	expect(t, "4.4 discard", `if header :contains ["from"] ["idiot@example.com"] {
   discard;
}`, "From: idiot@example.com\r\n\r\n", `discard`)
}

// RFC 5228 5: the tests, with the examples of each section.
func TestRFC5228_5_Tests(t *testing.T) {
	for _, c := range []struct {
		require, test string
		want          bool
	}{
		// 5.1
		{``, `address :is :all "from" "coyote@desert.example.org"`, true},
		{``, `address :is :all "from" "tim@example.com"`, false},
		{``, `address :is "to" "tim@example.com"`, true}, // any address of the field
		{``, `address :localpart :is "from" "coyote"`, true},
		{``, `address :domain :is "from" "DESERT.example.ORG"`, true}, // i;ascii-casemap is the default
		{``, `address :domain :comparator "i;octet" :is "from" "DESERT.example.ORG"`, false},
		{``, `address :contains ["cc", "bcc"] "me@"`, true},
		// 5.2 and 5.3
		{``, `allof (true, true)`, true}, {``, `allof (true, false)`, false},
		{``, `anyof (false, true)`, true}, {``, `anyof (false, false)`, false},
		// 5.4: the example, with the envelope of runOn.
		{`"envelope"`, `envelope :all :is "from" "coyote@desert.example.org"`, true},
		{`"envelope"`, `envelope :all :is "from" "tim@example.com"`, false},
		{`"envelope"`, `envelope :domain :is "to" "example.com"`, true},
		{`"envelope"`, `envelope :localpart "to" "me+sieve"`, true},
		// 5.5: "true if the headers listed ... exist"; all of them.
		{``, `exists "from"`, true}, {``, `exists ["From", "Date"]`, true}, {``, `exists ["from", "x-nope"]`, false},
		// 5.6, 5.8, 5.10
		{``, `false`, false}, {``, `true`, true}, {``, `not false`, true}, {``, `not exists "from"`, false},
		// 5.7: "X-Caffeine: C8H10N4O2" and the two tests with the empty key.
		{``, `header :is ["X-Caffeine"] [""]`, false},
		{``, `header :contains ["X-Caffeine"] [""]`, true},
		{``, `header :contains ["X-Nope"] [""]`, false}, // a field that is not there matches no key
		{``, `header :is "x-caffeine" "c8h10n4o2"`, true},
		// "leading and trailing whitespace is ignored", and 2.7.2: encoded
		// words are decoded.
		{``, `header :is "subject" "$$$ Make money fast $$$"`, true},
		{``, `header :matches "Subject" "*make*money*fast*"`, true},
		// 5.9: "neither :over nor :under is true if the size is equal".
		{``, `size :over 500K`, false}, {``, `size :under 500K`, true},
		{``, fmt.Sprintf(`size :over %d`, len(sample)), false}, {``, fmt.Sprintf(`size :under %d`, len(sample)), false},
		{``, fmt.Sprintf(`size :over %d`, len(sample)-1), true},
		// 2.7.3: the comparator example.
		{``, `header :contains :comparator "i;octet" "Subject" "MAKE MONEY FAST"`, false},
		{``, `header :contains :comparator "i;ascii-casemap" "Subject" "MAKE MONEY FAST"`, true},
		// 2.7.1: "*" and "?" and their escapes.
		{``, `header :matches "x-caffeine" "C?H10*"`, true},
		{``, `header :matches "x-caffeine" "C?H10"`, false},
		{``, `header :matches "x-caffeine" "\\C8H10N4O2"`, true}, // "\\C" in a script is \C to the matcher: a plain C
		{``, `header :matches "subject" "\\*"`, false},
		{``, `header :matches "subject" "$$$*$$$"`, true},
		// i;ascii-numeric (RFC 4790 9.1).
		{`"comparator-i;ascii-numeric"`, `header :is :comparator "i;ascii-numeric" "x-priority" "02"`, true},
		{`"comparator-i;ascii-numeric"`, `header :is :comparator "i;ascii-numeric" "x-priority" "3"`, false},
	} {
		if got := truth(t, c.require, c.test); got != c.want {
			t.Errorf("%s = %v, want %v", c.test, got, c.want)
		}
	}
}

// RFC 5228 2.4: numbers and strings.
func TestRFC5228_2_4_NumbersAndStrings(t *testing.T) {
	// 2.4.1: "K" is 2^10, "M" 2^20, "G" 2^30; case does not matter.
	for src, want := range map[string]int64{"1": 1, "1K": 1024, "2k": 2048, "1M": 1 << 20, "3g": 3 << 30, "0": 0} {
		nodes, err := parseSyntax([]byte("if size :over " + src + " { keep; }"))
		if err != nil || nodes[0].tests[0].args[1].num != want {
			t.Errorf("%s: %v %v", src, nodes, err)
		}
	}
	// 2.4.2: a quoted string with escapes; "an undefined escape sequence
	// ... is interpreted as if there were no backslash".
	nodes, err := parseSyntax([]byte(`redirect "a\"b\\c\d";`))
	if err != nil || nodes[0].args[0].text != `a"b\cd` {
		t.Errorf("quoted: %v %v", nodes, err)
	}
	// The multi-line form: a hash comment may follow "text:", lines that
	// start with a dot are dot-stuffed, and the string ends at ".".
	nodes, err = parseSyntax([]byte("reject text: # why\r\nline one\r\n..dotted\r\n\r\nlast\r\n.\r\n;"))
	if err != nil || nodes[0].args[0].text != "line one\r\n.dotted\r\n\r\nlast\r\n" {
		t.Errorf("multi-line: %q %v", nodes[0].args[0].text, err)
	}
	// 2.4.2.1: "a string list of one string is the same as the string".
	one, _ := parseSyntax([]byte(`if header :is ["a"] "b" { keep; }`))
	two, _ := parseSyntax([]byte(`if header :is "a" ["b"] { keep; }`))
	if strings.Join(one[0].tests[0].args[1].strings(), ",") != strings.Join(two[0].tests[0].args[1].strings(), ",") {
		t.Error("a list of one string is not the string")
	}
	// 2.3: comments, both kinds, anywhere white space may be.
	expect(t, "comments", "# first\r\nif /* inline */ true /* multi\r\nline */ { discard; # end\r\n}", sample, `discard`)
	// Deviation: RFC 5228 8.1 has CRLF as the only line break; a script
	// typed in a browser or kept in git has bare LF, and is taken too.
	expect(t, "bare LF", "require \"fileinto\";\n# comment\nfileinto text:\nx\n.\n;\n", sample, "fileinto \"x\\r\\n\"")
}

// RFC 5228 2.10.5, 3.2 and 8: what is an error before a script runs.
func TestRFC5228_Errors(t *testing.T) {
	for _, src := range []string{
		`fileinto "x";`,                   // 3.2: an extension that was not required
		`require "nonesuch";`,             // "MUST ... error" for an unknown capability
		`keep; require "fileinto";`,       // "Require commands ... must be at the top"
		`if true { require "fileinto"; }`, // also inside a block
		`frobnicate;`,                     // 2.10.5: an unknown command
		`if frobnicate { keep; }`,         // an unknown test
		`keep`,                            // 8.2: a command ends in ";" or a block
		`if true keep;`,                   // if needs a block
		`if { keep; }`,                    // and a test
		`else { keep; }`,                  // 3.1: else without if
		`if true { keep; } keep; elsif true { keep; }`, // elsif not straight after if
		`keep "x";`, // too many arguments
		`redirect;`, // too few
		`redirect ["a@b.example", "c@d.example"];`,             // a list where one string belongs
		`redirect "not an address";`,                           // 2.4.2.3
		`if size :over "1" { keep; }`,                          // a string where a number belongs
		`if size 1 { keep; }`,                                  // size needs :over or :under
		`if header :is :contains "a" "b" { keep; }`,            // two match types
		`if header :comparator "i;nonesuch" "a" "b" { keep; }`, // 2.7.3: an unknown comparator
		`if header :is "bad name" "b" { keep; }`,               // 2.4.2.2: not a header name
		`if header :is "a:b" "b" { keep; }`,
		`if exists { keep; }`,
		`if allof true { keep; }`, // allof takes a list in parentheses
		`if not (true, false) { keep; }`,
		`if true { keep; `, // an open block
		`if header :is "a" ["b", ] { keep; }`,
		`if header :is "a" [] { keep; }`, // 8.2: a list has at least one string
		`keep; "stray";`,
		`redirect "a@b.example"`,
		`if true { keep; } }`,
		`redirect "unterminated;`,
		`/* open comment`,
		`reject text:
never closed`,
		`if size :over 99999999999G { keep; }`,
		`if size :over 1T { keep; }`,
		"redirect \"a\x00b@c.example\";",
		"keep \xff;",
		`if address :user "to" "x" { keep; }`,                           // subaddress not required
		`if header :value "gt" "a" "b" { keep; }`,                       // relational not required
		`require "relational"; if header :value "xx" "a" "b" { keep; }`, // not a relation
		`if header :is :comparator "i;ascii-numeric" "a" "1" { keep; }`, // the comparator needs its require
		`require "comparator-i;ascii-numeric"; if header :contains :comparator "i;ascii-numeric" "a" "1" { keep; }`, // 2.7.3
		`require "envelope"; if envelope "received" "x" { keep; }`,                                                  // 5.4: not an envelope part
		`if header :localpart "from" "x" { keep; }`,                                                                 // an address part on header
	} {
		if s, err := Parse([]byte(src)); err == nil {
			t.Errorf("no error for: %s (%v)", src, s.Requires)
		} else if _, ok := err.(*Error); !ok {
			t.Errorf("%s: error %T is not *Error", src, err)
		}
	}
	// An error names its line.
	_, err := Parse([]byte("keep;\r\n\r\nif true {\r\n  frobnicate;\r\n}\r\n"))
	if e, ok := err.(*Error); !ok || e.Line != 4 || !strings.Contains(e.Error(), "line 4") {
		t.Errorf("line of an error: %v", err)
	}
}

// Limits: a script and a message cannot make the interpreter work without end.
func TestLimits(t *testing.T) {
	if _, err := Parse([]byte(strings.Repeat("keep;\n", MaxScriptSize/6+1))); err == nil {
		t.Error("a script over the size limit parsed")
	}
	deep := strings.Repeat("if true {", maxNesting+1) + strings.Repeat("}", maxNesting+1)
	if _, err := Parse([]byte(deep)); err == nil {
		t.Error("blocks nested too deep parsed")
	}
	// RFC 5228 2.10.5: at least 15 levels of nesting.
	fifteen := strings.Repeat("if true {", 15) + "discard;" + strings.Repeat("}", 15)
	expect(t, "15 levels", fifteen, sample, `discard`)
	nots := "if " + strings.Repeat("not ", maxNesting+2) + "true { keep; }"
	if _, err := Parse([]byte(nots)); err == nil {
		t.Error("tests nested too deep parsed")
	}
	// More redirects than MaxRedirects is an error at run time, and an
	// error keeps the message (2.10.6).
	var b strings.Builder
	for i := 0; i <= MaxRedirects; i++ {
		fmt.Fprintf(&b, "redirect \"r%d@x.example\";\n", i)
	}
	expect(t, "too many redirects", b.String(), sample, `keep ERROR`)
	// A pattern that would take a matcher without a bound a very long time.
	evil := `if header :matches "subject" "` + strings.Repeat("*a", 30) + `*b" { discard; }`
	start := time.Now()
	expect(t, "a hostile pattern", evil, "Subject: "+strings.Repeat("a", 5000)+"\r\n\r\n", `keep`)
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("the pattern took %v", d)
	}
}

// RFC 5228 10: the extended example.
func TestRFC5228_10_ExtendedExample(t *testing.T) {
	script := `#
# Example Sieve Filter
# Declare any optional features or extension used by the script
#
require ["fileinto"];

#
# Handle messages from known mailing lists
# Move messages from IETF filter discussion list to filter mailbox
#
if header :is "Sender" "owner-ietf-mta-filters@imc.org"
        {
        fileinto "filter";  # move to "filter" mailbox
        }
#
# Keep all messages to or from people in my company
#
elsif address :DOMAIN :is ["From", "To"] "example.com"
        {
        keep;               # keep in "In" mailbox
        }

#
# Try and catch unsolicited email.  If a message is not to me,
# or it contains a subject known to be spam, file it away.
#
elsif anyof (NOT address :all :contains
               ["To", "Cc", "Bcc"] "me@example.com",
             header :matches "subject"
               ["*make*money*fast*", "*university*dipl*mas*"])
        {
        fileinto "spam";   # move to "spam" mailbox
        }
else
        {
        # Move all other (non-company) mail to "personal"
        # mailbox.
        fileinto "personal";
        }
`
	script = strings.ReplaceAll(script, "\n", "\r\n")
	for msg, want := range map[string]string{
		"Sender: owner-ietf-mta-filters@imc.org\r\nTo: me@example.com\r\n\r\n":                    `fileinto "filter"`,
		"From: boss@example.com\r\nTo: someone@elsewhere.example\r\n\r\n":                         `keep`,
		"From: x@elsewhere.example\r\nTo: other@elsewhere.example\r\nSubject: hi\r\n\r\n":         `fileinto "spam"`,
		"From: x@elsewhere.example\r\nCc: me@example.com\r\nSubject: University Diplomas\r\n\r\n": `fileinto "spam"`,
		"From: x@elsewhere.example\r\nBcc: me@example.com\r\nSubject: lunch\r\n\r\n":              `fileinto "personal"`,
	} {
		expect(t, "the example", script, msg, want)
	}
}

func FuzzParseAndRun(f *testing.F) {
	for _, s := range []string{
		`require ["fileinto", "variables"]; if header :matches "subject" "[*] *" { fileinto "lists.${1}"; }`,
		"reject text:\r\nx\r\n.\r\n;", `if anyof (not true, allof (false)) { keep; } else { discard; }`,
		`require ["date","relational","index"]; if date :value "ge" :index 2 :zone "-0500" "received" "iso8601" "2007" { stop; }`,
		`require ["body","imap4flags","vacation"]; if body :content "text" :contains "x" { addflag "\\Seen"; vacation :days 3 "gone"; }`,
		`set :upper "a" "${b}${1}${`, "text:\n", "\"", "/*", "if size :over 1K {",
	} {
		f.Add(s, sample)
	}
	f.Fuzz(func(t *testing.T, script, msg string) {
		s, err := Parse([]byte(script))
		if err != nil {
			if _, ok := err.(*Error); !ok {
				t.Fatalf("error %T", err)
			}
			return
		}
		res := s.Run(Env{Raw: []byte(msg), From: "a@b.example", To: "c@d.example", Now: testNow})
		// Whatever a script does, the message ends up somewhere or was
		// turned away on purpose.
		if len(res.Actions) == 0 {
			t.Fatalf("no action at all for %q", script)
		}
		redirects := 0
		for _, a := range res.Actions {
			if a.Kind == Redirect {
				redirects++
				if !validAddress(a.Address) {
					t.Fatalf("redirect to %q", a.Address)
				}
			}
		}
		if redirects > MaxRedirects {
			t.Fatalf("%d redirects", redirects)
		}
	})
}
