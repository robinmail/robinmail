//go:build !webmail

package web

import (
	"strings"
	"testing"
)

// Without the "webmail" tag there is no webmail in the binary, and the
// server answers with the placeholder page.
func TestEmbeddedWithoutWebmail(t *testing.T) {
	if Embedded() != nil {
		t.Fatal("Embedded() without the webmail tag is not nil")
	}
	st := newSite(t)
	st.s = &Server{Hostname: "mail.example.test", Accounts: st.accounts, JMAP: st.s.JMAP, WithUser: st.s.WithUser, Static: Embedded()}
	st.h = st.s.Handler()
	rec := st.do("GET", tlsBase+"/mail/m1", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "The webmail is not built into this binary") {
		t.Errorf("placeholder: %d %s", rec.Code, rec.Body.String())
	}
}
