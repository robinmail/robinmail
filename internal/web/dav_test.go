package web

import (
	"context"
	"fmt"
	"github.com/robinmail/robinmail/internal/store"
	"net/http"
	"testing"
)

type davCtx struct{}

// /dav/ and the well-known URIs of RFC 6764: Basic auth with an app
// password of the "dav" scope (or the account password while the account
// has none), the challenge for programs, the cookie for the webmail's own
// pages; the origin check only where the cookie is.
func TestDAVAuth(t *testing.T) {
	st := newSite(t)
	st.s.DAV = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "dav for %v at %s", r.Context().Value(davCtx{}), r.URL.Path)
	})
	st.s.WithDAVUser = func(ctx context.Context, a store.Account) context.Context {
		return context.WithValue(ctx, davCtx{}, a.Address)
	}
	st.h = st.s.Handler()

	rec := st.do("PROPFIND", tlsBase+"/dav/", "")
	if rec.Code != 401 || rec.Header().Get("WWW-Authenticate") == "" {
		t.Fatalf("no login: %d %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
	// Thunderbird's own code sends "Origin: null".
	rec = st.do("PROPFIND", tlsBase+"/.well-known/caldav", "", basic("anna@example.test", "correct horse"), header("Origin", "null"))
	if rec.Code != 200 || rec.Body.String() != "dav for anna@example.test at /.well-known/caldav" {
		t.Fatalf("Basic with Origin null: %d %s", rec.Code, rec.Body.String())
	}
	c := st.in("anna@example.test", "correct horse")
	_, davOnly := st.appPassword(c, "correct horse", "Phone", "dav")
	_, jmapOnly := st.appPassword(c, "correct horse", "Script", "jmap")
	if rec := st.do("GET", tlsBase+"/dav/x", "", basic("anna@example.test", davOnly)); rec.Code != 200 {
		t.Errorf("a dav app password: %d", rec.Code)
	}
	if rec := st.do("GET", tlsBase+"/dav/x", "", basic("anna@example.test", jmapOnly)); rec.Code != 401 {
		t.Errorf("a jmap-only app password: %d", rec.Code)
	}
	if rec := st.do("GET", tlsBase+"/dav/x", "", basic("anna@example.test", "correct horse")); rec.Code != 401 {
		t.Errorf("the account password once there are app passwords: %d", rec.Code)
	}
	// The webmail's export link: the cookie, from its own origin.
	if rec := st.do("GET", tlsBase+"/dav/x", "", cookie(c), header("Sec-Fetch-Site", "same-origin")); rec.Code != 200 {
		t.Errorf("the cookie: %d", rec.Code)
	}
	if rec := st.do("PUT", tlsBase+"/dav/x", "", cookie(c), header("Origin", "https://evil.example")); rec.Code != 403 {
		t.Errorf("the cookie from another site: %d", rec.Code)
	}
	// The challenge also to what looks like a page of this origin: that is
	// what Thunderbird's calendar sends.
	if rec := st.do("PROPFIND", tlsBase+"/dav/x", "", header("Sec-Fetch-Site", "same-origin"), header("Origin", tlsBase)); rec.Code != 401 || rec.Header().Get("WWW-Authenticate") == "" {
		t.Errorf("Thunderbird's first request: %d %q", rec.Code, rec.Header().Get("WWW-Authenticate"))
	}
}
