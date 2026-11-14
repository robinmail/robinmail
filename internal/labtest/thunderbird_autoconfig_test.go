package labtest

import (
	"testing"
	"time"
)

// tbServer is one server of a configuration Thunderbird found.
type tbServer struct {
	Type       string `json:"type"`
	Hostname   string `json:"hostname"`
	Port       int    `json:"port"`
	SocketType int    `json:"socketType"`
	Auth       int    `json:"auth"`
	Username   string `json:"username"`
}

type tbConfig struct {
	Source               string     `json:"source"`
	Incoming             tbServer   `json:"incoming"`
	Outgoing             tbServer   `json:"outgoing"`
	IncomingAlternatives []tbServer `json:"incomingAlternatives"`
	OutgoingAlternatives []tbServer `json:"outgoingAlternatives"`
}

// TestThunderbirdAutoconfig is phase 8's autoconfig seen from the client:
// Thunderbird sets up an account from an address and a password alone.
// The lab's DNS has autoconfig.robin.test at robinmail, whose certificate
// names it; Mozilla's database and everything else outside the lab is out
// of reach, so what Thunderbird finds comes from robinmail.
func TestThunderbirdAutoconfig(t *testing.T) {
	lab(t)
	defer cell(t, "Thunderbird: account set up from the address alone (autoconfig)", tbCol)("")
	// A Thunderbird with an account to start with (the library wants a
	// main window with one), and a second user it knows nothing of.
	first := labUser(t, "tb-auto-a")
	second := labUser(t, "tb-auto-b")
	x := startThunderbird(t, first, password())

	// Thunderbird's numbers: socketType 3 is SSL/TLS, 2 STARTTLS; auth 3
	// is "normal password" (nsMsgSocketType, nsMsgAuthMethod).
	const (
		ssl, startTLS = 3, 2
		normal        = 3
	)
	check := func(what string, c tbConfig, user string) {
		t.Helper()
		if c.Source != "xml" {
			t.Errorf("%s: the configuration's source is %q, want \"xml\" (a config file, not a guess)", what, c.Source)
		}
		if c.Incoming != (tbServer{"imap", robinHost, 993, ssl, normal, user}) {
			t.Errorf("%s: incoming %+v", what, c.Incoming)
		}
		if c.Outgoing != (tbServer{"smtp", robinHost, 465, ssl, normal, user}) {
			t.Errorf("%s: outgoing %+v", what, c.Outgoing)
		}
		// The STARTTLS listeners are the second choice.
		if len(c.IncomingAlternatives) != 1 || c.IncomingAlternatives[0] != (tbServer{"imap", robinHost, 143, startTLS, normal, user}) {
			t.Errorf("%s: other incoming servers %+v", what, c.IncomingAlternatives)
		}
		// (Thunderbird leaves the placeholder in the user name of an
		// outgoing server it does not use.)
		if len(c.OutgoingAlternatives) == 1 {
			c.OutgoingAlternatives[0].Username = user
		}
		if len(c.OutgoingAlternatives) != 1 || c.OutgoingAlternatives[0] != (tbServer{"smtp", robinHost, 587, startTLS, normal, user}) {
			t.Errorf("%s: other outgoing servers %+v", what, c.OutgoingAlternatives)
		}
	}

	// The lookup at the provider, on its own: the file as Thunderbird
	// read it, the user name still the placeholder.
	var found tbConfig
	if err := x.tryFor(2*time.Minute, &found, `return describeConfig(await fromProvider(arguments[0]));`, second); err != nil {
		t.Fatalf("Thunderbird's lookup at the provider: %v", err)
	}
	t.Logf("FetchConfig.fromISP(%s): %+v", second, found)
	check("fromISP", found, "%EMAILADDRESS%")

	// The whole of it: find, fill in, log in to check, create the account.
	since := time.Now().Add(-time.Second)
	var made struct {
		Config  tbConfig `json:"config"`
		Account string   `json:"account"`
	}
	if err := x.tryFor(3*time.Minute, &made, `return await setUp(arguments[0], arguments[1], "Second User");`, second, password()); err != nil {
		t.Fatalf("Thunderbird's account setup for %s: %v", second, err)
	}
	t.Logf("account setup for %s made %q with %+v", second, made.Account, made.Config)
	check("the account setup", made.Config, second)
	if made.Account != second+" on "+robinHost+":993" {
		t.Errorf("the account that was made: %q", made.Account)
	}
	// The check of the password was a login at robinmail.
	waitFor(t, "robinmail to see the new account log in", 30*time.Second, func() bool {
		_, logins := imapTrace(t, since, second)
		return logins > 0
	})
	// And Thunderbird now has both accounts.
	var users []string
	x.eval(&users, `return MailServices.accounts.accounts.filter(a => a.incomingServer.type == "imap").map(a => a.incomingServer.username).sort();`)
	if len(users) != 2 {
		t.Errorf("Thunderbird's IMAP accounts: %v", users)
	}

	// An address of a domain robinmail does not have gets nothing from
	// it: no file under the hostname for a foreign address.
	var none *tbConfig
	if err := x.tryFor(2*time.Minute, &none, `try { return describeConfig(await fromProvider(arguments[0])); } catch (e) { return null; }`, "someone@postfix.test"); err != nil || none != nil {
		t.Errorf("a configuration for a domain that is not robinmail's: %+v %v", none, err)
	}
}
