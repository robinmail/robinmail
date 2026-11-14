package imap

import (
	"context"
	"errors"
	"time"
)

// Errors a Backend answers with. Anything else is "cannot tell right now".
var (
	// ErrAuth is a wrong name or password.
	ErrAuth = errors.New("imap: authentication failed")
	// ErrLimited is too many failed logins: try later.
	ErrLimited = errors.New("imap: too many failed logins")
	// ErrNoMailbox is a mailbox that does not exist (any more).
	ErrNoMailbox = errors.New("imap: no such mailbox")
	// ErrExists is a mailbox name that is taken.
	ErrExists = errors.New("imap: mailbox exists")
	// ErrHasChildren is a mailbox that cannot be deleted for its children.
	ErrHasChildren = errors.New("imap: mailbox has children")
	// ErrSpecial is a mailbox the server needs and will not delete.
	ErrSpecial = errors.New("imap: mailbox cannot be deleted")
	// ErrName is a mailbox name the store does not take.
	ErrName = errors.New("imap: invalid mailbox name")
	// ErrFlag is a flag the store does not take.
	ErrFlag = errors.New("imap: invalid flag")
	// ErrNoMessage is a message that is gone.
	ErrNoMessage = errors.New("imap: no such message")
	// ErrTooLarge is a message over the size limit.
	ErrTooLarge = errors.New("imap: message too large")
	// ErrLoop is a mailbox moved under itself.
	ErrLoop = errors.New("imap: a mailbox cannot be moved under itself")
)

// Backend is where the server gets its users.
type Backend interface {
	// Login checks a name and password, with whatever rate limits logins
	// have, and returns the user's mail.
	Login(ctx context.Context, remoteIP, username, password string) (User, error)
}

// Mailbox is one mailbox of a user. Mailboxes form a tree through
// ParentID (0 at the top); Name is the name within the parent, in UTF-8.
type Mailbox struct {
	ID       int64
	ParentID int64
	Name     string
	// Role is "inbox", "drafts", "sent", "junk", "trash", "archive" or
	// empty. The mailbox with role "inbox" is INBOX whatever its name.
	Role          string
	Subscribed    bool
	UIDValidity   uint32
	UIDNext       uint32
	Messages      int64
	Unseen        int64
	Deleted       int64
	Size          int64
	HighestModseq int64
}

// Message is a message in a mailbox, without its content.
type Message struct {
	UID uint32
	// ID names the content: it is what Open takes. Two messages with the
	// same ID in different mailboxes are one message with two labels.
	ID           int64
	Modseq       int64
	Size         int64
	InternalDate time.Time
	// Flags are keywords as JMAP has them: "$seen", "$flagged",
	// "$answered", "$draft", "$deleted" for the system flags, anything
	// else as the client named it, in lowercase.
	Flags []string
}

// View is a mailbox at one moment.
type View struct {
	UIDValidity uint32
	UIDNext     uint32
	// Modseq is the highest modseq any message of the mailbox can have at
	// this moment. Pass it to the next View as since.
	Modseq int64
	// UIDs are all UIDs in the mailbox, ascending.
	UIDs []uint32
	// Changed are the messages with a modseq above since, by UID.
	Changed []Message
}

// Summary is what the store remembers of a message's header, for SEARCH
// without opening the message. A field that was cut short or is not
// remembered is reported in Partial, and SEARCH then reads the message.
type Summary struct {
	From, To, Cc, Bcc, Subject string
	SentAt                     time.Time // zero without a Date field
	Partial                    bool
}

// UIDRange is an inclusive range of UIDs.
type UIDRange struct{ Lo, Hi uint32 }

// Revocable is a User whose login can be withdrawn while a session runs:
// the app password it logged in with is revoked, or the password changed.
// A backend's User that implements it has its sessions ended when that
// happens, with a BYE.
type Revocable interface {
	// Revoked returns a channel that is closed when the login no longer
	// stands, and a function that ends the watching.
	Revoked() (<-chan struct{}, func())
}

// TextSearcher is a User whose store has a full-text index. SEARCH BODY and
// SEARCH TEXT then ask the index instead of reading every message.
type TextSearcher interface {
	// SearchText returns the ids of the messages of a mailbox that have
	// text: in the body (bodyOnly) or anywhere. indexed is false when the
	// index cannot answer for this text; the messages are read then.
	SearchText(ctx context.Context, mailbox int64, text string, bodyOnly bool) (ids []int64, indexed bool, err error)
}

// UIDPair is a message's UID before and after a copy.
type UIDPair struct{ From, To uint32 }

// How Store changes flags.
const (
	FlagsAdd = iota
	FlagsRemove
	FlagsReplace
)

// User is one logged-in user's mail. All methods are safe for use by
// several sessions at once.
type User interface {
	Username() string

	Mailboxes(ctx context.Context) ([]Mailbox, error)
	// Create makes a mailbox under parent (0: the top) and returns its id.
	Create(ctx context.Context, parent int64, name string) (int64, error)
	// Rename gives a mailbox a new parent and name.
	Rename(ctx context.Context, id, parent int64, name string) error
	// Delete removes a mailbox and takes its messages out of it.
	Delete(ctx context.Context, id int64) error
	Subscribe(ctx context.Context, id int64, subscribed bool) error

	// View reads a mailbox: all UIDs, and the messages changed since a
	// modseq (0: all).
	View(ctx context.Context, mailbox, since int64) (View, error)
	// Open returns a whole message.
	Open(ctx context.Context, id int64) ([]byte, error)
	// Summaries returns what is remembered of the headers of messages.
	Summaries(ctx context.Context, ids []int64) (map[int64]Summary, error)

	// Append stores a message and returns its UID and the mailbox's
	// UIDVALIDITY.
	Append(ctx context.Context, mailbox int64, raw []byte, flags []string, date time.Time) (uid, uidValidity uint32, err error)
	// Store changes the flags of the messages with a UID in uids. With
	// unchangedSince >= 0, messages with a higher modseq are left alone
	// and their UIDs returned.
	Store(ctx context.Context, mailbox int64, uids []UIDRange, mode int, flags []string, unchangedSince int64) (modified []uint32, err error)
	// Copy puts messages into another mailbox, and with move takes them
	// out of this one. It returns the target's UIDVALIDITY and each
	// message's UID in both.
	Copy(ctx context.Context, from int64, uids []UIDRange, to int64, move bool) (uidValidity uint32, pairs []UIDPair, err error)
	// Expunge removes the messages with "$deleted" from the mailbox; with
	// uids, only those among them.
	Expunge(ctx context.Context, mailbox int64, uids []UIDRange) error

	// Changes returns a channel that gets a value after something of the
	// user's mail changed, from this session or anywhere else, and a
	// function that ends the subscription.
	Changes() (<-chan struct{}, func())
}
