package store

// Tests written from RFC 5322 3.6.4 (the Message-ID, In-Reply-To and
// References fields) and RFC 8621 section 3 (Threads), as far as thread
// assignment follows them.

import (
	"strings"
	"testing"
	"time"
)

// The conversation of RFC 5322 appendix A.2, "Reply Messages": John writes
// to Mary, Mary replies, John replies to the reply.
const (
	a2First = "From: John Doe <jdoe@machine.example>\r\n" +
		"To: Mary Smith <mary@example.net>\r\n" +
		"Subject: Saying Hello\r\n" +
		"Date: Fri, 21 Nov 1997 09:55:06 -0600\r\n" +
		"Message-ID: <1234@local.machine.example>\r\n" +
		"\r\n" +
		"This is a message just to say hello.\r\n" +
		"So, \"Hello\".\r\n"
	a2Reply = "From: Mary Smith <mary@example.net>\r\n" +
		"To: John Doe <jdoe@machine.example>\r\n" +
		"Reply-To: \"Mary Smith: Personal Account\" <smith@home.example>\r\n" +
		"Subject: Re: Saying Hello\r\n" +
		"Date: Fri, 21 Nov 1997 10:01:10 -0600\r\n" +
		"Message-ID: <3456@example.net>\r\n" +
		"In-Reply-To: <1234@local.machine.example>\r\n" +
		"References: <1234@local.machine.example>\r\n" +
		"\r\n" +
		"This is a reply to your hello.\r\n"
	a2ReplyToReply = "To: \"Mary Smith: Personal Account\" <smith@home.example>\r\n" +
		"From: John Doe <jdoe@machine.example>\r\n" +
		"Subject: Re: Saying Hello\r\n" +
		"Date: Fri, 21 Nov 1997 11:00:00 -0600\r\n" +
		"Message-ID: <abcd.1234@local.machine.example>\r\n" +
		"In-Reply-To: <3456@example.net>\r\n" +
		"References: <1234@local.machine.example> <3456@example.net>\r\n" +
		"\r\n" +
		"This is a reply to your reply.\r\n"
)

// ids returns what summarize takes from a header: the message's own id and
// every id it is linked to.
func ids(header string) (string, []string) {
	s := summarize([]byte(header + "\r\nbody\r\n"))
	return s.messageID, s.refs
}

// RFC 5322 3.6.4 with the messages of appendix A.2: "The 'In-Reply-To:'
// field will contain the contents of the 'Message-ID:' field of the message
// to which this one is a reply ... The 'References:' field will contain the
// contents of the parent's 'References:' field (if any) followed by the
// contents of the parent's 'Message-ID:' field". And: "the angle bracket
// characters are not part of the msg-id".
func TestRFC5322_3_6_4_AppendixA2(t *testing.T) {
	for _, tc := range []struct {
		name, raw, id string
		refs          []string
	}{
		{"A.2 first", a2First, "1234@local.machine.example", []string{"1234@local.machine.example"}},
		{"A.2 reply", a2Reply, "3456@example.net", []string{"3456@example.net", "1234@local.machine.example"}},
		{"A.2 reply to the reply", a2ReplyToReply, "abcd.1234@local.machine.example",
			[]string{"abcd.1234@local.machine.example", "1234@local.machine.example", "3456@example.net"}},
	} {
		s := summarize([]byte(tc.raw))
		want(t, tc.name+": Message-ID", s.messageID, tc.id)
		want(t, tc.name+": linked ids", s.refs, tc.refs)
	}
}

// RFC 5322 3.6.4:
//
//	message-id  = "Message-ID:" msg-id CRLF
//	in-reply-to = "In-Reply-To:" 1*msg-id CRLF
//	references  = "References:" 1*msg-id CRLF
//	msg-id      = [CFWS] "<" id-left "@" id-right ">" [CFWS]
//	id-left     = dot-atom-text / obs-id-left
//	id-right    = dot-atom-text / no-fold-literal / obs-id-right
//
// "The 'References:' and 'In-Reply-To:' fields each contain one or more
// unique message identifiers, optionally separated by CFWS": white space,
// folding and comments.
func TestRFC5322_3_6_4_Syntax(t *testing.T) {
	for _, tc := range []struct {
		name, header, id string
		refs             []string
	}{
		{"every atext character on both sides",
			"Message-ID: <a.!#$%&'*+-/=?^_`{|}~.z@b.!#$%&'*+-/=?^_`{|}~.y>\r\n",
			"a.!#$%&'*+-/=?^_`{|}~.z@b.!#$%&'*+-/=?^_`{|}~.y", []string{"a.!#$%&'*+-/=?^_`{|}~.z@b.!#$%&'*+-/=?^_`{|}~.y"}},
		{"no-fold-literal on the right",
			"Message-ID: <1234@[192.0.2.1]>\r\n", "1234@[192.0.2.1]", []string{"1234@[192.0.2.1]"}},
		{"identifiers are case-sensitive",
			"Message-ID: <ABC@x.example>\r\nReferences: <abc@x.example>\r\n", "ABC@x.example", []string{"ABC@x.example", "abc@x.example"}},
		{"CFWS around the identifier",
			"Message-ID:   \t<1@x.example>  \r\n", "1@x.example", []string{"1@x.example"}},
		{"a comment after the identifier",
			"Message-ID: <1@x.example> (sent from my desk)\r\n", "1@x.example", []string{"1@x.example"}},
		{"no white space between identifiers",
			"Message-ID: <3@x.example>\r\nReferences: <1@x.example><2@x.example>\r\n", "3@x.example",
			[]string{"3@x.example", "1@x.example", "2@x.example"}},
		{"References folded over lines",
			"Message-ID: <4@x.example>\r\nReferences: <1@x.example>\r\n <2@x.example>\r\n\t<3@x.example>\r\n", "4@x.example",
			[]string{"4@x.example", "1@x.example", "2@x.example", "3@x.example"}},
		{"comments between identifiers",
			"Message-ID: <3@x.example>\r\nReferences: <1@x.example> (the first) <2@x.example> (the (nested) second)\r\n", "3@x.example",
			[]string{"3@x.example", "1@x.example", "2@x.example"}},
		// A comment is not an identifier, whatever it contains.
		{"an identifier inside a comment",
			"Message-ID: <3@x.example>\r\nReferences: <1@x.example> (not <fake@x.example> \\) nor <other@x.example>) <2@x.example>\r\n",
			"3@x.example", []string{"3@x.example", "1@x.example", "2@x.example"}},
		{"several parents in In-Reply-To",
			"Message-ID: <3@x.example>\r\nIn-Reply-To: <1@x.example> <2@x.example>\r\n", "3@x.example",
			[]string{"3@x.example", "1@x.example", "2@x.example"}},
		// 4.5.4, which a reader MUST accept: obs-in-reply-to and
		// obs-references = *(phrase / msg-id).
		{"obsolete In-Reply-To with a phrase",
			"Message-ID: <2@x.example>\r\nIn-Reply-To: Your message of Fri, 21 Nov 1997 <1@x.example>\r\n", "2@x.example",
			[]string{"2@x.example", "1@x.example"}},
		{"obsolete phrase with a quoted string",
			"Message-ID: <2@x.example>\r\nIn-Reply-To: \"the <fake@x.example> one\" <1@x.example>\r\n", "2@x.example",
			[]string{"2@x.example", "1@x.example"}},
		// 1.2.2 and 2.2: field names are case-insensitive.
		{"field names in another case",
			"MESSAGE-ID: <2@x.example>\r\nin-reply-to: <1@x.example>\r\nreferences: <0@x.example>\r\n", "2@x.example",
			[]string{"2@x.example", "0@x.example", "1@x.example"}},
		// "The 'Message-ID:' field contains a single unique message
		// identifier." A second one is not this message's.
		{"two identifiers in Message-ID",
			"Message-ID: <1@x.example> <2@x.example>\r\n", "1@x.example", []string{"1@x.example"}},
		{"no identification fields",
			"Subject: none\r\n", "", nil},
		{"In-Reply-To without Message-ID",
			"In-Reply-To: <1@x.example>\r\n", "", []string{"1@x.example"}},
	} {
		id, refs := ids(tc.header)
		want(t, tc.name+": Message-ID", id, tc.id)
		want(t, tc.name+": linked ids", refs, tc.refs)
	}
}

// RFC 8621 section 3: "Every Email MUST belong to a Thread, even if it is
// the only Email in the Thread." And, with the messages of RFC 5322
// appendix A.2: replies join the thread of the original, and the Thread
// lists its Emails "sorted by the receivedAt date of the Email, oldest
// first".
func TestRFC8621_3_ThreadOfAppendixA2(t *testing.T) {
	s := open(t)
	a := account(t, s)
	at := time.Date(1997, 11, 21, 16, 0, 0, 0, time.UTC)
	first := deliver(t, s, a, Message{Raw: []byte(a2First), ReceivedAt: at})
	reply := deliver(t, s, a, Message{Raw: []byte(a2Reply), ReceivedAt: at.Add(6 * time.Minute)})
	last := deliver(t, s, a, Message{Raw: []byte(a2ReplyToReply), ReceivedAt: at.Add(65 * time.Minute)})
	alone := deliver(t, s, a, Message{Raw: []byte("Subject: no ids at all\r\n\r\nbody\r\n"), ReceivedAt: at})

	if first.ThreadID == 0 || alone.ThreadID == 0 || alone.ThreadID == first.ThreadID {
		t.Errorf("threads %d and %d", first.ThreadID, alone.ThreadID)
	}
	want(t, "thread of the reply", reply.ThreadID, first.ThreadID)
	want(t, "thread of the reply to the reply", last.ThreadID, first.ThreadID)
	got, err := s.ThreadEmails(ctx, a.ID, first.ThreadID)
	if err != nil {
		t.Fatal(err)
	}
	want(t, "emails of the thread", got, []int64{first.ID, reply.ID, last.ID})
	got, _ = s.ThreadEmails(ctx, a.ID, alone.ThreadID)
	want(t, "a thread of one", got, []int64{alone.ID})
}

// RFC 8621 section 3, condition 1: two messages belong together if "an
// identical message id appears in both messages in any of the Message-Id,
// In-Reply-To, and References header fields". Each case is a pair that
// shares one id through a different combination of fields.
func TestRFC8621_3_SharedMessageID(t *testing.T) {
	s := open(t)
	a := account(t, s)
	hdr := func(lines ...string) []byte {
		return []byte(strings.Join(lines, "\r\n") + "\r\nSubject: x\r\n\r\nbody\r\n")
	}
	for _, tc := range []struct {
		name          string
		first, second []byte
	}{
		{"Message-ID and In-Reply-To", hdr("Message-ID: <p1@x.example>"), hdr("Message-ID: <c1@x.example>", "In-Reply-To: <p1@x.example>")},
		{"Message-ID and References", hdr("Message-ID: <p2@x.example>"), hdr("Message-ID: <c2@x.example>", "References: <p2@x.example>")},
		{"In-Reply-To and Message-ID: the reply came first",
			hdr("Message-ID: <c3@x.example>", "In-Reply-To: <p3@x.example>"), hdr("Message-ID: <p3@x.example>")},
		{"References and References: siblings of a parent never received",
			hdr("Message-ID: <c4@x.example>", "References: <p4@x.example>"), hdr("Message-ID: <d4@x.example>", "References: <p4@x.example>")},
		{"In-Reply-To and References",
			hdr("Message-ID: <c5@x.example>", "In-Reply-To: <p5@x.example>"), hdr("Message-ID: <d5@x.example>", "References: <o5@x.example> <p5@x.example>")},
		{"Message-ID and Message-ID: the same message twice", hdr("Message-ID: <p6@x.example>"), hdr("Message-ID: <p6@x.example>")},
		{"no Message-ID on the reply", hdr("Message-ID: <p7@x.example>"), hdr("In-Reply-To: <p7@x.example>")},
	} {
		first := deliver(t, s, a, Message{Raw: tc.first})
		second := deliver(t, s, a, Message{Raw: tc.second})
		if first.ThreadID != second.ThreadID {
			t.Errorf("%s: threads %d and %d", tc.name, first.ThreadID, second.ThreadID)
		}
	}
	// Identifiers are compared exactly: another case is another message.
	upper := deliver(t, s, a, Message{Raw: hdr("Message-ID: <CASE@x.example>")})
	lower := deliver(t, s, a, Message{Raw: hdr("Message-ID: <r@x.example>", "In-Reply-To: <case@x.example>")})
	if upper.ThreadID == lower.ThreadID {
		t.Error("identifiers that differ in case were taken as the same")
	}
}

// RFC 8621 section 3: the "threadId" of an Email is immutable. "If messages
// are delivered out of order ... The arrival of a third Email may provide
// the missing references to join them all together into a single Thread.
// Since the 'threadId' of an Email is immutable, if the server wishes to
// merge the Threads, it MUST handle this by deleting and reinserting (with a
// new Email id) the Emails that change 'threadId'."
//
// We do not wish to: the two threads stay apart, the third message joins
// one of them, and no Email changes its thread or its id.
func TestRFC8621_3_ThreadIDImmutable(t *testing.T) {
	s := open(t)
	a := account(t, s)
	hdr := func(lines ...string) []byte {
		return []byte(strings.Join(lines, "\r\n") + "\r\nSubject: x\r\n\r\nbody\r\n")
	}
	one := deliver(t, s, a, Message{Raw: hdr("Message-ID: <one@x.example>")})
	two := deliver(t, s, a, Message{Raw: hdr("Message-ID: <two@x.example>")})
	if one.ThreadID == two.ThreadID {
		t.Fatal("unrelated messages share a thread")
	}
	link := deliver(t, s, a, Message{Raw: hdr("Message-ID: <link@x.example>", "References: <one@x.example> <two@x.example>")})
	if link.ThreadID != one.ThreadID && link.ThreadID != two.ThreadID {
		t.Errorf("the linking message is in thread %d, not in %d or %d", link.ThreadID, one.ThreadID, two.ThreadID)
	}
	for _, e := range []Email{one, two} {
		now, err := s.Email(ctx, a.ID, e.ID)
		if err != nil || now.ThreadID != e.ThreadID {
			t.Errorf("email %d: thread %d, was %d (%v)", e.ID, now.ThreadID, e.ThreadID, err)
		}
	}
	// Moving and flagging do not change it either.
	if _, err := s.UpdateEmail(ctx, a.ID, one.ID, Update{AddMailboxes: []int64{trash}, RemoveMailboxes: []int64{inbox}, AddKeywords: []string{"$seen"}}); err != nil {
		t.Fatal(err)
	}
	if now, _ := s.Email(ctx, a.ID, one.ID); now.ThreadID != one.ThreadID {
		t.Errorf("thread changed from %d to %d by a move", one.ThreadID, now.ThreadID)
	}
}

// Deviation from the suggestion in RFC 8621 section 3. "The exact algorithm
// for determining whether two Emails belong to the same Thread is not
// mandated"; the RFC suggests two conditions, a shared message id and "after
// stripping automatically added prefixes such as 'Fwd:', 'Re:',
// '[List-Tag]', etc., and ignoring white space, the subjects are the same".
// We apply only the first: a reply stays in its thread when the subject
// changes. Comparing subjects needs a list of prefixes in every language,
// and the thread is fixed at delivery, so a wrong split could not be undone.
func TestRFC8621_3_Deviation_SubjectNotCompared(t *testing.T) {
	s := open(t)
	a := account(t, s)
	first := deliver(t, s, a, Message{Raw: []byte("Message-ID: <s1@x.example>\r\nSubject: Lunch on Tuesday\r\n\r\nbody\r\n")})
	changed := deliver(t, s, a, Message{Raw: []byte("Message-ID: <s2@x.example>\r\nIn-Reply-To: <s1@x.example>\r\nSubject: Something else entirely\r\n\r\nbody\r\n")})
	want(t, "thread of a reply with another subject", changed.ThreadID, first.ThreadID)
	// And the other way round: the same subject without a shared id is
	// not a thread.
	same := deliver(t, s, a, Message{Raw: []byte("Message-ID: <s3@x.example>\r\nSubject: Re: Lunch on Tuesday\r\n\r\nbody\r\n")})
	if same.ThreadID == first.ThreadID {
		t.Error("a message joined a thread on its subject alone")
	}
}

// RFC 8621 section 3: emailIds are "sorted by the 'receivedAt' date of the
// Email, oldest first. If two Emails have an identical date, the sort is
// server dependent but MUST be stable (sorting by id is recommended)."
func TestRFC8621_3_ThreadOrder(t *testing.T) {
	s := open(t)
	a := account(t, s)
	at := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	m := func(id string, received time.Time) Email {
		return deliver(t, s, a, Message{Raw: msg(id, id, "order-root@x.example"), ReceivedAt: received})
	}
	// Stored in this order, received in another.
	late := m("o1@x.example", at.Add(3*time.Hour))
	early := m("o2@x.example", at.Add(1*time.Hour))
	tie1 := m("o3@x.example", at.Add(2*time.Hour))
	tie2 := m("o4@x.example", at.Add(2*time.Hour))
	oldest := m("o5@x.example", at)
	want := []int64{oldest.ID, early.ID, tie1.ID, tie2.ID, late.ID}
	for i := 0; i < 3; i++ { // stable: the same answer every time
		got, err := s.ThreadEmails(ctx, a.ID, late.ThreadID)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != len(want) {
			t.Fatalf("thread has %v, want %v", got, want)
		}
		for j := range got {
			if got[j] != want[j] {
				t.Fatalf("thread order %v, want %v", got, want)
			}
		}
	}
}
