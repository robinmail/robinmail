package sieve

import (
	"os"
	"strings"
	"testing"
	"time"
)

// RFC 5229 3: variable references in strings.
func TestRFC5229_3_References(t *testing.T) {
	// The example of the section. Variable names are case-insensitive.
	script := `require ["variables", "ereject"];
set "honorific"  "Mr";
set "first_name" "Wile";
set "last_name"  "Coyote";
set "vacation" text:
Dear ${HONORIFIC} ${last_name},
I'm out, please leave a message after the meep.
.
;
ereject "${vacation}";`
	expect(t, "the example", script, sample, `ereject "Dear Mr Coyote,\r\nI'm out, please leave a message after the meep.\r\n"`)
	// "&%${}!" stays "as the empty string is an illegal identifier";
	// "${doh!}" stays "as "!" is illegal in identifiers".
	for in, want := range map[string]string{
		`&%${}!`:            `&%${}!`,
		`${doh!}`:           `${doh!}`,
		`${unknown}`:        ``, // "unknown variables are replaced by the empty string"
		`${company}`:        `ACME`,
		`${COMPANY}`:        `ACME`,
		`${${company}}`:     `${ACME}`, // "the first "}" ends the reference": ${${company} is no reference, so it is text
		`a${company}b${0}c`: `aACMEbc`,
		`${`:                `${`,
		`${company`:         `${company`,
	} {
		script := `require ["variables", "reject"]; set "company" "ACME"; reject "` + in + `";`
		got := mustParse(t, script).Run(Env{Raw: []byte(sample), Now: testNow})
		if len(got.Actions) != 1 || got.Actions[0].Reason != want {
			t.Errorf("%s became %q, want %q", in, got.Actions[0].Reason, want)
		}
	}
	// Without the require, "${" is text.
	expect(t, "no variables without require", `require "reject"; reject "${company}";`, sample, `reject "${company}"`)
}

// RFC 5229 3.2: match variables, with the example of the section.
func TestRFC5229_3_2_MatchVariables(t *testing.T) {
	script := `require ["fileinto", "variables"];

if header :matches "List-ID" "*<*@*" {
    fileinto "INBOX.lists.${2}"; stop;
}

# Imagine the header
# Subject: [acme-users] [fwd] version 1.0 is out
if header :matches "Subject" "[*] *" {
    # ${1} will hold "acme-users",
    # ${2} will hold "[fwd] version 1.0 is out"
    fileinto "INBOX.lists.${1}"; stop;
}

# Imagine the header
# To: coyote@ACME.Example.COM
if address :matches ["To", "Cc"] ["coyote@**.com",
        "wile@**.com"] {
    # ${0} is the matched address
    # ${1} is always the empty string
    # ${2} is part of the domain name ("ACME.Example")
    fileinto "INBOX.business.${2}"; stop;
} else {
    # Control wouldn't reach this block if any match succeeded
    fileinto "INBOX";
}`
	expect(t, "List-ID", script, "List-ID: Dev <dev@lists.example>\r\n\r\n", `fileinto "INBOX.lists.dev"`)
	expect(t, "Subject", script, "Subject: [acme-users] [fwd] version 1.0 is out\r\n\r\n", `fileinto "INBOX.lists.acme-users"`)
	expect(t, "To", script, "To: coyote@ACME.Example.COM\r\n\r\n", `fileinto "INBOX.business.ACME.Example"`)
	expect(t, "nothing", script, "To: other@x.example\r\n\r\n", `fileinto "INBOX"`)
	// "${0} is the whole value"; a match that fails leaves the variables
	// of the last one that held; other match types set none.
	expect(t, "0 and a failed match", `require ["variables", "reject"];
if header :matches "x-caffeine" "C8*N4*" { }
if header :matches "x-caffeine" "nothing*" { }
if header :contains "x-caffeine" "H10" { }
reject "${0}|${1}|${2}|${3}";`, sample, `reject "C8H10N4O2|H10|O2|"`)
	// "*" is as short as it can be.
	expect(t, "shortest", `require ["variables", "reject"]; if header :matches "x-caffeine" "*H*" { reject "${1}|${2}"; }`,
		"X-Caffeine: aHbHc\r\n\r\n", `reject "a|bHc"`)
}

// RFC 5229 4: set and its modifiers, the examples of 4.1.
func TestRFC5229_4_Set(t *testing.T) {
	for set, want := range map[string]string{
		`set "b" "${a}";`:                    "juMBlEd lETteRS",
		`set :length "b" "${a}";`:            "15",
		`set :lower "b" "${a}";`:             "jumbled letters",
		`set :upperfirst "b" "${a}";`:        "JuMBlEd lETteRS",
		`set :upperfirst :lower "b" "${a}";`: "Jumbled letters",
		`set :lower :upperfirst "b" "${a}";`: "Jumbled letters", // the order written does not matter
		`set :quotewildcard "b" "Rock*";`:    `Rock\*`,
		`set :length :upper "b" "${a}";`:     "15",
	} {
		script := `require ["variables", "reject"]; set "a" "juMBlEd lETteRS"; ` + set + ` reject "${b}";`
		got := mustParse(t, script).Run(Env{Raw: []byte(sample), Now: testNow})
		if got.Actions[0].Reason != want {
			t.Errorf("%s gives %q, want %q", set, got.Actions[0].Reason, want)
		}
	}
	for _, bad := range []string{
		`require "variables"; set :lower :upper "b" "x";`, // "modifiers with the same precedence ... is an error"
		`require "variables"; set "0" "x";`,               // a match variable cannot be set
		`require "variables"; set "a.b" "x";`,
		`require "variables"; set "" "x";`,
		`set "a" "x";`,
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("no error for %s", bad)
		}
	}
	// 5: the string test, with the example.
	expect(t, "string", `require ["variables", "reject"];
set "state" "${state} pending";
if string :matches " ${state} " "* pending *" { reject "pending"; }`, sample, `reject "pending"`)
	// 6: a value is cut at 4000 octets, not in the middle of a character.
	long := mustParse(t, `require ["variables", "reject"]; set "a" "`+strings.Repeat("é", 3000)+`"; set :length "n" "${a}"; reject "${n}";`)
	if got := long.Run(Env{Raw: []byte(sample), Now: testNow}); got.Actions[0].Reason != "2000" {
		t.Errorf("a long value has %s characters", got.Actions[0].Reason)
	}
}

// RFC 5230 4: the vacation action and its arguments, with the examples.
func TestRFC5230_Vacation(t *testing.T) {
	expect(t, "4.1 days and addresses", `require "vacation";
vacation :days 23 :addresses ["tjs@example.edu",
                              "ts4z@landru.example.edu"]
"I'm away until October 19.  If it's an emergency, call 911, I guess." ;`, sample,
		`vacation days=23 subject="" from="" addresses=[tjs@example.edu ts4z@landru.example.edu] mime=false handle="" "I'm away until October 19.  If it's an emergency, call 911, I guess." | keep`)
	subject := `require ["vacation", "variables"];
if header :matches "subject" "*" {
    vacation :subject "Automatic response to: ${1}"
             "I'm away -- send mail to foo in my absence";
}`
	expect(t, "4.2 subject", subject, "Subject: lunch\r\n\r\n",
		`vacation days=0 subject="Automatic response to: lunch" from="" addresses=[] mime=false handle="" "I'm away -- send mail to foo in my absence" | keep`)
	expect(t, "4.3 from, and 4.6 handle", `require "vacation"; vacation :handle "ran-away" :from "Me <me@example.com>" "I'm out";`, sample,
		`vacation days=0 subject="" from="Me <me@example.com>" addresses=[] mime=false handle="ran-away" "I'm out" | keep`)
	mime := `require "vacation";
vacation :mime text:
Content-Type: multipart/alternative; boundary=foo

--foo

I'm at the beach relaxing.  Mmmm, surf...

--foo
Content-Type: text/html; charset=us-ascii

<HTML><BODY><P>I'm at the beach relaxing.</BODY></HTML>

--foo--
.
;`
	got := mustParse(t, mime).Run(Env{Raw: []byte(sample), Now: testNow})
	if v := got.Actions[0].Vacation; !v.MIME || !strings.HasPrefix(v.Reason, "Content-Type: multipart/alternative; boundary=foo\r\n\r\n--foo\r\n") {
		t.Errorf("4.5 mime: %+v", v)
	}
	// 4.1: "the minimum value [of :days] is 1" hence 0 becomes 1.
	expect(t, "days 0", `require "vacation"; vacation :days 0 "x";`, sample, `vacation days=1 subject="" from="" addresses=[] mime=false handle="" "x" | keep`)
	// 4.7: vacation leaves the implicit keep alone, may run once, and
	// does not go with reject (RFC 5429 2.1.1).
	expect(t, "twice", `require "vacation"; vacation "a"; vacation "b";`, sample, `keep ERROR`)
	expect(t, "with reject", `require ["vacation", "reject"]; vacation "a"; reject "b";`, sample, `keep ERROR`)
	expect(t, "with discard", `require "vacation"; vacation "a"; discard;`, sample,
		`vacation days=0 subject="" from="" addresses=[] mime=false handle="" "a" | discard`)
	for _, bad := range []string{`vacation "x";`, `require "vacation"; vacation;`, `require "vacation"; vacation :days "3" "x";`,
		`require "vacation"; vacation :from "nobody" "x";`, `require "vacation"; vacation :nonesuch "x";`} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("no error for %s", bad)
		}
	}
}

// RFC 5231: :count and :value, with the example of section 6. (The RFC
// writes "elseif" there; the language has elsif.)
func TestRFC5231_Relational(t *testing.T) {
	script := `require ["relational", "comparator-i;ascii-numeric", "fileinto"];

if header :value "lt" :comparator "i;ascii-numeric"
          ["x-priority"] ["3"]
{
   fileinto "Priority";
}
elsif address :count "gt" :comparator "i;ascii-numeric"
           ["to"] ["5"]
{
   # everything with more than 5 recipients in the "to" field
   # is considered SPAM
   fileinto "SPAM";
}
elsif address :value "gt" :all :comparator "i;ascii-casemap"
           ["from"] ["M"]
{
   fileinto "From N-Z";
} else {
   fileinto "From A-M";
}

if allof ( address :count "eq" :comparator "i;ascii-numeric"
                   ["to", "cc"] ["1"] ,
           address :all :comparator "i;ascii-casemap"
                   ["to", "cc"] ["me@foo.example.com"] )
{
   fileinto "Only me";
}`
	expect(t, "priority", script, "X-Priority: 2\r\nFrom: a@x.example\r\n\r\n", `fileinto "Priority"`)
	expect(t, "many recipients", script, "To: a@x.example, b@x.example, c@x.example, d@x.example, e@x.example, f@x.example\r\nFrom: zed@x.example\r\n\r\n", `fileinto "SPAM"`)
	expect(t, "from N-Z", script, "From: zed@x.example\r\nTo: a@x.example, b@x.example\r\n\r\n", `fileinto "From N-Z"`)
	expect(t, "from A-M, only me", script, "From: anna@x.example\r\nTo: me@foo.example.com\r\n\r\n", `fileinto "From A-M" | fileinto "Only me"`)
	// 4.2: :count of header counts the fields; a field that is not there counts 0.
	for test, want := range map[string]bool{
		`header :count "eq" :comparator "i;ascii-numeric" "received" "2"`:                 true,
		`header :count "ge" :comparator "i;ascii-numeric" ["received", "x-caffeine"] "3"`: true,
		`header :count "eq" :comparator "i;ascii-numeric" "x-nope" "0"`:                   true,
		`header :value "ge" "x-caffeine" "c8"`:                                            true,
		`header :value "ne" "x-caffeine" "C8H10N4O2"`:                                     false,
		// RFC 4790 9.1.1: a string that does not start with a digit is
		// larger than every number.
		`header :value "gt" :comparator "i;ascii-numeric" "x-caffeine" "999999"`: true,
	} {
		if got := truth(t, `"relational", "comparator-i;ascii-numeric"`, test); got != want {
			t.Errorf("%s = %v", test, got)
		}
	}
}

// RFC 5232: flags, with the examples of sections 3 to 5.
func TestRFC5232_Flags(t *testing.T) {
	expect(t, "3.1 setflag and the implicit keep", `require "imap4flags"; if size :over 0 { setflag "\\Deleted"; }`, sample, `keep flags=\Deleted`)
	boss := `require ["imap4flags", "variables", "fileinto"];
if header :contains "from" "boss@frobnitzm.example.edu" {
  setflag "flagvar" "\\Flagged";
  fileinto :flags "${flagvar}" "INBOX.From Boss";
}`
	expect(t, "3.1 a flag variable", boss, "From: boss@frobnitzm.example.edu\r\n\r\n", `fileinto "INBOX.From Boss" flags=\Flagged`)
	// 3.2: the three ways to write the same addflag.
	for _, add := range []string{
		`addflag "flagvar" "\\Deleted"; addflag "flagvar" "\\Answered";`,
		`addflag "flagvar" ["\\Deleted", "\\Answered"];`,
		`addflag "flagvar" "\\Deleted \\Answered";`,
	} {
		expect(t, "3.2 "+add, `require ["imap4flags", "variables"]; `+add+` keep :flags "${flagvar}";`, sample, `keep flags=\Deleted,\Answered`)
	}
	removal := `require ["imap4flags", "variables", "fileinto"];
if header :contains "Disposition-Notification-To" "mel@example.com" {
    addflag "flagvar" "$MDNRequired";
}
if header :contains "from" "imap@cac.washington.example.edu" {
    removeflag "flagvar" "$MDNRequired";
    fileinto :flags "${flagvar}" "INBOX.imap-list";
}`
	expect(t, "3.3 removeflag", removal, "From: imap@cac.washington.example.edu\r\nDisposition-Notification-To: mel@example.com\r\n\r\n", `fileinto "INBOX.imap-list"`)
	// The internal variable: flags add up without doubles, whatever their case.
	expect(t, "the internal variable", `require "imap4flags"; addflag "\\Seen $label1"; addflag ["\\seen", "$label2"]; removeflag "$LABEL1"; keep;`, sample, `keep flags=\Seen,$label2`)
	// 4: hasflag, with the internal variable holding "A B".
	for test, want := range map[string]bool{
		`hasflag "b"`:           true,
		`hasflag ["c", "A"]`:    true,
		`hasflag "c"`:           false,
		`hasflag :contains "b"`: true,
		`hasflag :is "b A"`:     true, // a key with two flags: either is enough
		`hasflag :count "eq" :comparator "i;ascii-numeric" "2"`: true,
	} {
		script := `require ["imap4flags", "relational", "comparator-i;ascii-numeric"]; setflag "A B"; if ` + test + ` { discard; }`
		if got := runOn(t, script, sample) == "discard"; got != want {
			t.Errorf("%s = %v", test, got)
		}
	}
	expect(t, "hasflag on a variable", `require ["imap4flags", "variables"]; set "MyVar" "NonJunk Junk gnus-forward $Forwarded NotJunk JunkRecorded $Junk $NotJunk";
if hasflag :contains "MyVar" "Junk" { discard; }`, sample, `discard`)
	// 5: :flags on keep and fileinto replaces the internal variable for that action.
	expect(t, "5 :flags", `require ["imap4flags", "fileinto"]; setflag "\\Seen"; fileinto :flags ["\\Flagged", "$Work"] "x"; fileinto "y";`, sample,
		`fileinto "x" flags=\Flagged,$Work | fileinto "y" flags=\Seen`)
	for _, bad := range []string{`setflag "\\Seen";`, `require "imap4flags"; keep :flags;`, `require "imap4flags"; setflag "v" "\\Seen";`,
		`require ["imap4flags", "variables"]; setflag ["a", "b"] "\\Seen";`} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("no error for %s", bad)
		}
	}
}

// RFC 5233 4: :user and :detail, with the examples of the section and of 6.
func TestRFC5233_Subaddress(t *testing.T) {
	script := `require ["envelope", "subaddress", "fileinto"];

# In this example the same user account receives mail for both
# "ken@example.com" and "postmaster@example.com"

# File all messages to postmaster into a single mailbox,
# ignoring the :detail part.
if envelope :user "to" "postmaster" {
    fileinto "inbox.postmaster";
    stop;
}

# File mailing list messages (subscribed as "ken+mta-filters").
if envelope :detail "to" "mta-filters" {
    fileinto "inbox.ietf-mta-filters";
}

# Redirect all mail sent to "ken+foo".
if envelope :detail "to" "foo" {
    redirect "ken@example.net";
}`
	run := func(to string) string {
		return describe(mustParse(t, script).Run(Env{Raw: []byte(sample), To: to, Now: testNow}))
	}
	for to, want := range map[string]string{
		"postmaster+x@example.com":    `fileinto "inbox.postmaster"`,
		"ken+mta-filters@example.com": `fileinto "inbox.ietf-mta-filters"`,
		"ken+foo@example.com":         `redirect ken@example.net`,
		"ken@example.com":             `keep`,
	} {
		if got := run(to); got != want {
			t.Errorf("to %s: %s, want %s", to, got, want)
		}
	}
	// "ken+sieve@example.org": :user is "ken", :detail "sieve". For
	// "ken+@example.org" :detail is the empty string. For
	// "ken@example.org" there is no detail: "the test evaluates to false".
	for _, c := range []struct {
		to, test string
		want     bool
	}{
		{"ken+sieve@example.org", `envelope :user "to" "ken"`, true},
		{"ken+sieve@example.org", `envelope :detail "to" "sieve"`, true},
		{"ken+sieve@example.org", `envelope :localpart "to" "ken+sieve"`, true},
		{"ken+@example.org", `envelope :detail :is "to" ""`, true},
		{"ken@example.org", `envelope :detail :is "to" ""`, false},
		{"ken@example.org", `envelope :detail :contains "to" ""`, false},
		{"ken@example.org", `envelope :user "to" "ken"`, true},
		{"ken+a+b@example.org", `envelope :detail "to" "a+b"`, true},
	} {
		s := mustParse(t, `require ["envelope", "subaddress"]; if `+c.test+` { discard; }`)
		if got := describe(s.Run(Env{Raw: []byte(sample), To: c.to, Now: testNow})) == "discard"; got != c.want {
			t.Errorf("%s with %s = %v", c.test, c.to, got)
		}
	}
	// In a header too.
	if !truth(t, `"subaddress"`, `address :user "to" "roadrunner"`) {
		t.Error("address :user")
	}
}

// RFC 3894: :copy, with its two examples; RFC 5490 3 and 4: mailboxexists
// and :create; RFC 5429 2: reject and ereject.
func TestRFC3894_5490_5429(t *testing.T) {
	expect(t, "RFC 3894 fileinto :copy keeps the implicit keep", `require ["copy", "fileinto"];
fileinto :copy "incoming";

# ... more processing follows ...`, sample, `fileinto "incoming" | keep`)
	expect(t, "RFC 3894 redirect :copy", `require ["copy"];
redirect :copy "bob@example.com";`, sample, `redirect bob@example.com | keep`)
	if _, err := Parse([]byte(`require "fileinto"; fileinto :copy "x";`)); err == nil {
		t.Error(":copy without its require")
	}

	exists := `require ["fileinto", "reject", "mailbox"];
if mailboxexists "Partners" {
   fileinto "Partners";
} else {
   reject "This message was not accepted by the Mailstore";
}`
	expect(t, "RFC 5490 3.1 mailboxexists", exists, sample, `fileinto "Partners"`)
	expect(t, "RFC 5490 3.1: all of them", `require "mailbox"; if mailboxexists ["Partners", "Nope"] { discard; }`, sample, `keep`)
	expect(t, "RFC 5490 3.2 :create", `require ["fileinto", "mailbox"]; fileinto :create "Partners/New";`, sample, `fileinto "Partners/New" create`)

	big := `require ["reject"];

if size :over 100K {
    reject text:
Your message is too big.  If you want to send me a big attachment,
put it on a public web site and send me a URL.
.
;
}`
	huge := sample + strings.Repeat("x", 101<<10)
	expect(t, "RFC 5429 2.2 reject", big, huge, `reject "Your message is too big.  If you want to send me a big attachment,\r\nput it on a public web site and send me a URL.\r\n"`)
	expect(t, "RFC 5429 2.2 reject, small message", big, sample, `keep`)
	expect(t, "RFC 5429 2.1 ereject", `require ["ereject"];

if address "from" "someone@example.com" {
    ereject "I no longer accept mail from this address";
}`, "From: someone@example.com\r\n\r\n", `ereject "I no longer accept mail from this address"`)
	// 2.1.1 and 2.2.1: reject does not go with keep, fileinto or redirect,
	// in either order; a script that tries keeps the message.
	for _, script := range []string{
		`require "reject"; keep; reject "no";`, `require "reject"; reject "no"; keep;`,
		`require ["reject", "fileinto"]; fileinto "x"; reject "no";`, `require "reject"; reject "no"; redirect "a@b.example";`,
		`require ["reject", "ereject"]; reject "no"; ereject "no";`,
	} {
		expect(t, "reject with another action", script, sample, `keep ERROR`)
	}
	expect(t, "reject and discard", `require "reject"; reject "no"; discard;`, sample, `reject "no" | discard`)
}

// RFC 5260: date, currentdate and :index, with the examples of 4.4, 5 and 6.
func TestRFC5260_Date(t *testing.T) {
	office := `require ["date", "relational", "fileinto"];
if allof(header :is "from" "boss@example.com",
         date :value "ge" :originalzone "date" "hour" "09",
         date :value "lt" :originalzone "date" "hour" "17")
{ fileinto "urgent"; }

if anyof(date :is "received" "weekday" "0",
         date :is "received" "weekday" "6")
{ fileinto "weekend"; }`
	at := func(date, received string) string {
		return "From: boss@example.com\r\nDate: " + date + "\r\nReceived: by x; " + received + "\r\n\r\n"
	}
	env := func(script, msg string) string {
		return describe(mustParse(t, script).Run(Env{Raw: []byte(msg), Now: testNow, Zone: time.UTC}))
	}
	for msg, want := range map[string]string{
		// 09:06 in the sender's zone, on a Tuesday.
		at("Tue, 1 Apr 1997 09:06:31 -0800", "Tue, 1 Apr 1997 17:10:00 +0000"): `fileinto "urgent"`,
		// 17:00 there is too late; received on a Saturday (UTC).
		at("Fri, 13 Nov 2026 17:00:00 -0800", "Sat, 14 Nov 2026 01:00:00 +0000"): `fileinto "weekend"`,
		// In the original zone it is 08:59, though 16:59 in UTC.
		at("Tue, 1 Apr 1997 08:59:00 -0800", "Tue, 1 Apr 1997 17:10:00 +0000"): `keep`,
	} {
		if got := env(office, msg); got != want {
			t.Errorf("%q: %s, want %s", msg[:60], got, want)
		}
	}
	july := `require ["date", "relational", "vacation"];
if allof(currentdate :value "ge" "date" "2007-06-30",
         currentdate :value "le" "date" "2007-07-07")
{ vacation :days 7  "I'm away during the first week in July."; }`
	in := func(now time.Time) string {
		return describe(mustParse(t, july).Run(Env{Raw: []byte(sample), Now: now}))
	}
	if got := in(time.Date(2007, 7, 1, 12, 0, 0, 0, time.UTC)); !strings.Contains(got, "vacation days=7") {
		t.Errorf("in the first week of July: %s", got)
	}
	if got := in(time.Date(2007, 7, 8, 0, 0, 1, 0, time.UTC)); got != "keep" {
		t.Errorf("after it: %s", got)
	}
	expect(t, "5: a mailbox per month", `require ["date", "variables", "fileinto"];
if currentdate :matches "month" "*" { set "month" "${1}"; }
if currentdate :matches "year"  "*" { set "year"  "${1}"; }
fileinto "${month}-${year}";`, sample, `fileinto "11-2026"`)
	// 6: the example (without the comma the RFC has after the last
	// argument, which the grammar does not allow). The second Received
	// field of sample is 2026-11-15T09:59 +01:00.
	cutoff := `require ["date", "relational", "index"];
if date :value "gt" :index 2 :zone "-0500" "received"
        "iso8601" "2007-02-26T09:00:00-05:00"
{ redirect "aftercutoff@example.org"; }`
	expect(t, "6 :index", cutoff, sample, `redirect aftercutoff@example.org`)

	// 4.2: every date part, of sample's Date field in its own zone and
	// of the current date in the zone of testNow.
	for test, want := range map[string]bool{
		`date :originalzone "date" "year" "1997"`:                            true,
		`date :originalzone "date" "month" "04"`:                             true,
		`date :originalzone "date" "day" "01"`:                               true,
		`date :originalzone "date" "date" "1997-04-01"`:                      true,
		`date :originalzone "date" "julian" "50539"`:                         true,
		`date :originalzone "date" "hour" "09"`:                              true,
		`date :originalzone "date" "minute" "06"`:                            true,
		`date :originalzone "date" "second" "31"`:                            true,
		`date :originalzone "date" "time" "09:06:31"`:                        true,
		`date :originalzone "date" "iso8601" "1997-04-01T09:06:31-08:00"`:    true,
		`date :originalzone "date" "std11" "Tue, 1 Apr 1997 09:06:31 -0800"`: true,
		`date :originalzone "date" "zone" "-0800"`:                           true,
		`date :originalzone "date" "weekday" "2"`:                            true,
		`date :zone "+0100" "date" "hour" "18"`:                              true,
		`date :zone "+0100" "date" "zone" "+0100"`:                           true,
		`currentdate "date" "2026-11-14"`:                                    true,
		`currentdate "zone" "+0100"`:                                         true,
		`currentdate :zone "-1000" "date" "2026-11-14"`:                      true,
		`currentdate :zone "+1400" "date" "2026-11-15"`:                      true,
		`currentdate "weekday" "6"`:                                          true,
		`date "x-caffeine" "year" "1997"`:                                    false, // no date in it: no value
		`date "x-nope" "year" "1997"`:                                        false,
		// 6: :index and :last.
		`date :index 1 "received" "day" "14"`:                       true,
		`date :index 1 :last "received" "day" "15"`:                 true,
		`date :index 3 "received" "day" "14"`:                       false,
		`header :index 2 :contains "received" "origin.example.org"`: true,
		`header :index 1 :contains "received" "origin.example.org"`: false,
		`address :index 1 :last :is "to" "tim@example.com"`:         true,
	} {
		if got := truth(t, `"date", "index"`, test); got != want {
			t.Errorf("%s = %v", test, got)
		}
	}
	for _, bad := range []string{
		`if currentdate "year" "2026" { keep; }`,
		`require "date"; if currentdate "century" "20" { keep; }`,
		`require "date"; if currentdate :zone "CET" "year" "2026" { keep; }`,
		`require "date"; if currentdate :originalzone "year" "2026" { keep; }`,
		`require "date"; if date :zone "+0100" :originalzone "date" "year" "2026" { keep; }`,
		`require "date"; if date :index 1 "date" "year" "2026" { keep; }`, // index was not required
		`require ["date", "index"]; if date :index 0 "date" "year" "2026" { keep; }`,
		`require ["date", "index"]; if date :last "date" "year" "2026" { keep; }`,
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("no error for %s", bad)
		}
	}
}

// RFC 5173: the body test and its transforms, with the examples of 5.
func TestRFC5173_Body(t *testing.T) {
	multipart := "From: a@x.example\r\nContent-Type: multipart/mixed; boundary=b\r\n\r\n" +
		"This is a multi-part message in MIME format.\r\n" +
		"--b\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: base64\r\n\r\n" +
		"VGhlIG1pc3NpbGUgaXMgcmVhZHku\r\n" + // "The missile is ready."
		"--b\r\nContent-Type: text/html\r\n\r\n<p>The <b>project</b> schedule</p>\r\n" +
		"--b\r\nContent-Type: application/octet-stream\r\nContent-Transfer-Encoding: base64\r\n\r\nY29vcmRpbmF0ZXM=\r\n" + // "coordinates"
		"--b--\r\n"
	script := `require ["body", "fileinto"];

# Save any message with any text MIME part that contains the
# words "missile" or "coordinates" in the "secrets" folder.

if body :content "text" :contains ["missile", "coordinates"] {
        fileinto "secrets";
}

# Save any message with an audio/mp3 MIME part in
# the "jukebox" folder.

if body :content "audio/mp3" :contains "" {
        fileinto "jukebox";
}`
	expect(t, "5.2 :content", script, multipart, `fileinto "secrets"`)
	expect(t, "5.2 :content, an mp3", script, "Content-Type: audio/mp3\r\n\r\nxx\r\n", `fileinto "jukebox"`)
	for test, want := range map[string]bool{
		// 5.1: :raw is the body as it was sent, transfer encoding and all.
		`body :raw :contains "MIME"`:            true,
		`body :raw :contains "VGhlIG1pc3NpbGU"`: true,
		`body :raw :contains "missile"`:         false,
		`body :raw :contains "From:"`:           false, // the header is not the body
		// 5.3: :text is the default: text parts, decoded, HTML without markup.
		`body :contains "missile"`:                                              true,
		`body :text :contains "project schedule"`:                               true,
		`body :text :contains "<b>"`:                                            false,
		`body :text :contains "coordinates"`:                                    false, // not a text part
		`body :content "text/html" :contains "<b>project</b>"`:                  true,  // :content is the part as it is
		`body :content "text/plain" :contains "project"`:                        false,
		`body :content "application" :contains "coordinates"`:                   true,
		`body :content "" :contains "coordinates"`:                              true, // "" is every part
		`body :content ["image", "application/octet-stream"] :is "coordinates"`: true,
		`body :matches "*MISSILE*"`:                                             true,
		`body :comparator "i;octet" :contains "MISSILE"`:                        false,
	} {
		script := `require "body"; if ` + test + ` { discard; }`
		if got := runOn(t, script, multipart) == "discard"; got != want {
			t.Errorf("%s = %v", test, got)
		}
	}
	// 5: "the body test MUST NOT set match variables".
	expect(t, "no match variables", `require ["body", "variables", "reject"];
if header :matches "from" "*@*" { }
if body :matches "*missile*" { reject "${1}"; }`, multipart, `reject "a"`)
	if _, err := Parse([]byte(`if body :contains "x" { keep; }`)); err == nil {
		t.Error("body without its require")
	}
}

// The script the webmail's Filters page writes (web-ng/projects/jmap/src/lib/sieve/rules.ts).
// The file is the fixture of the page's own test: what the page writes is
// what runs here.
func TestWebmailRules(t *testing.T) {
	src, err := os.ReadFile("../../web-ng/projects/jmap/src/lib/sieve/testdata/rules.sieve")
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{string(src), strings.ReplaceAll(string(src), "\n", "\r\n")} {
		for msg, want := range map[string]string{
			"From: Billing <billing@shop.example>\r\nSubject: Your Invoice 7\r\n\r\n":         `fileinto "Work/Invoices" create flags=\Seen`,
			"From: billing@shop.example\r\nSubject: hello\r\n\r\n":                            `keep`,
			"From: a@x.example\r\nCc: dev@lists.example\r\nSubject: patch\r\n\r\n":            `fileinto "Lists" create | redirect archive@example.org | keep`,
			"From: a@x.example\r\nSubject: [DEV] patch\r\n\r\n":                               `fileinto "Lists" create | redirect archive@example.org | keep`,
			"From: a@x.example\r\nSubject: A special \\ \"offer\" for you\r\n\r\n":            `discard`,
			"From: billing@shop.example\r\nTo: dev@lists.example\r\nSubject: invoice\r\n\r\n": `fileinto "Work/Invoices" create flags=\Seen`,
		} {
			expect(t, "the webmail's rules", text, msg, want)
		}
	}
}
