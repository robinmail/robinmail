package address

// Tests written from RFC 5321 4.1.2 (Mailbox) and RFC 5322 3.4.1
// (addr-spec). This package names the mailboxes robinmail itself has; it
// accepts a subset of what the RFCs allow, and never anything they do not.

import (
	"regexp"
	"strings"
	"testing"
)

// The grammars as regular expressions.
//
// RFC 5321 4.1.2, without the Quoted-string and address-literal forms:
//
//	Mailbox    = Local-part "@" Domain
//	Dot-string = Atom *("." Atom)
//	Domain     = sub-domain *("." sub-domain)
//	sub-domain = Let-dig [Ldh-str]
//
// RFC 5322 3.4.1 and 3.2.3, without CFWS, quoted strings, domain literals
// and the obsolete forms:
//
//	addr-spec     = dot-atom-text "@" dot-atom-text
//	dot-atom-text = 1*atext *("." 1*atext)
const (
	atext     = "[A-Za-z0-9!#$%&'*+/=?^_`{|}~-]"
	dotString = atext + `+(?:\.` + atext + `+)*`
	subDomain = `[A-Za-z0-9](?:[A-Za-z0-9-]*[A-Za-z0-9])?`
)

var (
	rfc5321Mailbox  = regexp.MustCompile(`^` + dotString + `@` + subDomain + `(?:\.` + subDomain + `)*$`)
	rfc5322AddrSpec = regexp.MustCompile(`^` + dotString + `@` + dotString + `$`)
)

// conforms reports whether a parsed address is a Mailbox of RFC 5321 and an
// addr-spec of RFC 5322, within the size limits of RFC 5321 4.5.3.1.
func conforms(a Address) bool {
	s := a.String()
	return rfc5321Mailbox.MatchString(s) && rfc5322AddrSpec.MatchString(s) &&
		len(a.Local) <= 64 && len(a.Domain) <= 255 && len(s)+2 <= 256
}

// RFC 5321 4.1.2 and RFC 5322 3.4.1: addresses in the form both RFCs share,
// including the ones RFC 5321's examples use, parse; and what parses
// conforms to both grammars.
func TestRFC5321_4_1_2_Mailbox(t *testing.T) {
	for _, in := range []string{
		"Smith@bar.com", "Jones@foo.com", "Admin.MRC@foo.com", "JQP@bar.com", "userc@d.bar.org", // RFC 5321 appendix D
		"jdoe@machine.example", "mary@example.net", "john.q.public@example.com", // RFC 5322 appendix A
		"a@b", "user@1.example", "user@a-b.example", "user@123.example", "user@xn--bcher-kva.example",
		"first.last@example.com", "user+tag@example.com", "user_name@example.com", "user-name@example.com",
		"-user@example.com", "_@example.com", "+@example.com", "0@example.com",
	} {
		a, err := Parse(in)
		if err != nil {
			t.Errorf("Parse(%q): %v", in, err)
			continue
		}
		if !conforms(a) {
			t.Errorf("Parse(%q) = %q, which is not a Mailbox and addr-spec", in, a)
		}
		if !strings.EqualFold(a.String(), in) {
			t.Errorf("Parse(%q) = %q", in, a)
		}
	}
	// Neither a Mailbox nor an addr-spec.
	for _, in := range []string{
		"", "@", "Smith", "Smith@", "@bar.com", "Smith@bar.com@baz.com",
		".Smith@bar.com", "Smith.@bar.com", "Sm..ith@bar.com", // not a Dot-string
		"Sm ith@bar.com", "Sm,ith@bar.com", "Sm(i)th@bar.com", "Sm<i>th@bar.com", "Sm[i]th@bar.com",
		"Sm:ith@bar.com", "Sm;ith@bar.com", "Sm\\ith@bar.com", "Sm\"ith@bar.com", // specials are not atext
		"Smith@bar_baz.com", // "the underscore character is not permitted"
		"Smith@-bar.com", "Smith@bar-.com", "Smith@bar..com", "Smith@.bar.com", "Smith@bar.com.",
		"Smith@bar.c*m", "Smith@bar com", "Smith@[bar.com", "<Smith@bar.com>",
		"Sm\x00ith@bar.com", "Sm\x7fith@bar.com", "Smith@bar.com\r\nRCPT TO:<x@y>", "Smith\r\n@bar.com",
	} {
		if a, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) = %q, want an error", in, a)
		}
	}
}

// RFC 5321 4.5.3.1.1 to 4.5.3.1.3: a local-part is at most 64 octets, a
// domain at most 255, and a path, which is the address in angle brackets, at
// most 256. The path limit is the one that binds: it leaves 254 octets for
// the address.
//
// Deviation: a domain label stops at 63 octets and a domain at 253. Those
// are the limits of the DNS (RFC 1035 2.3.4: a name is 255 octets on the
// wire, which is 253 as text), and a mailbox in a domain that cannot be in
// the DNS cannot receive mail.
func TestRFC5321_4_5_3_1_Limits(t *testing.T) {
	l63 := strings.Repeat("d", 63)
	d252 := l63 + "." + l63 + "." + l63 + "." + l63[:60]
	d253 := l63 + "." + l63 + "." + l63 + "." + l63[:61]
	if len(d252) != 252 || len(d253) != 253 {
		t.Fatal("test domains have the wrong length")
	}
	if !ValidDomain(d253) || ValidDomain(d253+"d") {
		t.Error("ValidDomain: the limit is not 253 octets")
	}
	for in, want := range map[string]bool{
		strings.Repeat("l", 64) + "@example.com": true,
		strings.Repeat("l", 65) + "@example.com": false,
		"user@" + l63 + ".com":                   true,
		"user@" + l63 + "d.com":                  false,
		"u@" + d252:                              true,  // a path of 256 octets
		"us@" + d252:                             false, // 257
		"u@" + d253:                              false, // 257
	} {
		a, err := Parse(in)
		if (err == nil) != want {
			t.Errorf("Parse(local of %d, domain of %d octets): err = %v, want ok = %v",
				strings.IndexByte(in, '@'), len(in)-strings.IndexByte(in, '@')-1, err, want)
		}
		if err == nil && !conforms(a) {
			t.Errorf("%q does not conform", a)
		}
	}
}

// RFC 5321 2.4 and 4.1.2: domains are case-insensitive. Local-parts "MAY be
// case-sensitive", but "a host that expects to receive mail SHOULD avoid
// defining mailboxes where ... the Local-part is case-sensitive". We define
// none: every spelling of an address names the same mailbox, the lower-case
// one. 4.5.1: "postmaster" is case-insensitive by rule.
func TestRFC5321_2_4_CaseInsensitiveMailboxes(t *testing.T) {
	for in, want := range map[string]string{
		"Smith@BAR.COM":      "smith@bar.com",
		"SMITH@bar.com":      "smith@bar.com",
		"smith@Bar.Com":      "smith@bar.com",
		"Postmaster@foo.com": "postmaster@foo.com",
		"POSTMASTER@FOO.COM": "postmaster@foo.com",
	} {
		a, err := Parse(in)
		if err != nil || a.String() != want {
			t.Errorf("Parse(%q) = %q, %v; want %q", in, a, err, want)
		}
	}
}

// Deviations, all one decision: robinmail has no mailboxes with these names,
// so Parse refuses them and a message for one is answered "no such user".
//
//   - RFC 5321 4.1.2 allows a Quoted-string local-part, and says "a host
//     that expects to receive mail SHOULD avoid defining mailboxes where
//     the Local-part requires (or uses) the Quoted-string form".
//   - RFC 5322 3.2.3's atext has more characters than letters, digits and
//     ". _ + -". The others ("!", "#", "/", "=", ...) mean something to
//     shells, URLs and file systems, and an address ends up in all three.
//   - RFC 5321 4.1.3 address literals: we serve the domains in the domain
//     table, not IP addresses.
//   - RFC 5322 3.4.1 allows comments and folding white space around the
//     parts of an addr-spec in a header field. This parser is for envelope
//     and account addresses, which have none (RFC 5321 4.1.2).
//   - RFC 6531 non-ASCII addresses: accounts are ASCII.
//
// Parse does take white space around the whole address, for an address
// typed on a command line.
func TestRFC5321_4_1_2_Deviation_Subset(t *testing.T) {
	for _, in := range []string{
		`"Joe\,Smith"@example.com`, `"john doe"@example.com`, `""@example.com`,
		"user!name@example.com", "user#1@example.com", "a/b@example.com", "a=b@example.com", "user's@example.com",
		"{user}@example.com", "user|name@example.com", "user~@example.com", "user?@example.com", "50%@example.com",
		"$user@example.com", "user&co@example.com", "*@example.com", "^user@example.com", "`user`@example.com",
		"user@[192.0.2.1]", "user@[IPv6:2001:db8::1]",
		"user(comment)@example.com", "user@(comment)example.com", "user @example.com", "user@ example.com",
		"Pelé@example.com", "user@bücher.example", "用户@例子.广告",
	} {
		if a, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) = %q, want an error", in, a)
		}
	}
	if a, err := Parse("  Smith@bar.com\n"); err != nil || a.String() != "smith@bar.com" {
		t.Errorf("Parse with surrounding white space = %q, %v", a, err)
	}
}

// Whatever Parse accepts is a Mailbox of RFC 5321 and an addr-spec of RFC
// 5322, and is its own canonical form.
func FuzzParseConforms(f *testing.F) {
	for _, s := range []string{"Smith@bar.com", "a@b", "\"q\"@example.com", "user@[192.0.2.1]", "a..b@c", "user@-x.example", " x@y \n"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		a, err := Parse(s)
		if err != nil {
			return
		}
		if !conforms(a) {
			t.Errorf("Parse(%q) = %q, which does not conform", s, a)
		}
		if again, err := Parse(a.String()); err != nil || again != a {
			t.Errorf("Parse(%q) = %q, %v: not canonical", a, again, err)
		}
	})
}
