//go:build webmail

package web

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// The built webmail, as "make" embeds it, served by the real handler: the
// entry page for every route of the app, its assets cacheable forever, and
// nothing in the page that the Content-Security-Policy would refuse.
func TestEmbeddedWebmail(t *testing.T) {
	app := Embedded()
	index, err := fs.ReadFile(app, "index.html")
	if err != nil {
		t.Fatalf("the embedded webmail has no index.html: %v", err)
	}
	page := string(index)

	// script-src is 'self' (through default-src): no inline script, no
	// inline event handler, nothing from another host.
	for _, m := range regexp.MustCompile(`(?is)<script\b([^>]*)>(.*?)</script>`).FindAllStringSubmatch(page, -1) {
		if !strings.Contains(m[1], "src=") || strings.TrimSpace(m[2]) != "" {
			t.Errorf("inline script in index.html: %s", m[0])
		}
	}
	if regexp.MustCompile(`(?i)\son[a-z]+\s*=`).MatchString(page) {
		t.Errorf("inline event handler in index.html")
	}
	if regexp.MustCompile(`(?i)(src|href)\s*=\s*["']?(https?:)?//`).MatchString(page) {
		t.Errorf("index.html loads from another host:\n%s", page)
	}
	// The policy says base-uri 'none': a <base> element would be refused,
	// and the page's relative addresses would then point somewhere else.
	if regexp.MustCompile(`(?i)<base\b`).MatchString(page) {
		t.Errorf("index.html has a <base> element")
	}

	st := newSite(t)
	st.s = &Server{Hostname: "mail.example.test", Accounts: st.accounts, JMAP: st.s.JMAP, WithUser: st.s.WithUser, Static: app}
	st.h = st.s.Handler()

	// Routes of the app are not files: they get the entry page, never cached.
	for _, path := range []string{"/", "/login", "/mail", "/mail/m1", "/mail/m1/t12?unread=1", "/no/such/route"} {
		rec := st.do("GET", tlsBase+path, "")
		if rec.Code != 200 || rec.Body.String() != page {
			t.Errorf("%s: %d, not the entry page", path, rec.Code)
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("%s: Cache-Control %q", path, cc)
		}
		csp := rec.Header().Get("Content-Security-Policy")
		for _, want := range []string{"default-src 'self'", "frame-src 'self'", "connect-src 'self'", "object-src 'none'", "frame-ancestors 'none'"} {
			if !strings.Contains(csp, want) {
				t.Errorf("%s: CSP lacks %q: %s", path, want, csp)
			}
		}
	}

	// Every asset the entry page names exists and may be kept for a year:
	// its name changes with its content.
	refs := regexp.MustCompile(`(?:src|href)="(/[^"]+)"`).FindAllStringSubmatch(page, -1)
	var scripts, styles int
	for _, m := range refs {
		rec := st.do("GET", tlsBase+m[1], "")
		if rec.Code != 200 {
			t.Errorf("%s: %d", m[1], rec.Code)
			continue
		}
		if !strings.HasPrefix(m[1], "/assets/") {
			continue
		}
		if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
			t.Errorf("%s: Cache-Control %q", m[1], cc)
		}
		ct := rec.Header().Get("Content-Type")
		switch {
		case strings.HasSuffix(m[1], ".js"):
			scripts++
			if !strings.HasPrefix(ct, "text/javascript") {
				t.Errorf("%s: Content-Type %q", m[1], ct)
			}
		case strings.HasSuffix(m[1], ".css"):
			styles++
			if !strings.HasPrefix(ct, "text/css") {
				t.Errorf("%s: Content-Type %q", m[1], ct)
			}
		}
	}
	if scripts == 0 || styles == 0 {
		t.Errorf("index.html names %d scripts and %d style sheets", scripts, styles)
	}

	// All files under assets/ are served, also those with a leading
	// underscore, which a go:embed pattern without "all:" would drop.
	entries, err := fs.ReadDir(app, "assets")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if rec := st.do("GET", tlsBase+"/assets/"+e.Name(), ""); rec.Code != 200 || !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
			t.Errorf("/assets/%s: %d %q", e.Name(), rec.Code, rec.Header().Get("Cache-Control"))
		}
	}
	if rec := st.do("GET", tlsBase+"/assets/index-00000000.js", ""); rec.Code != 404 {
		t.Errorf("an asset of an older build: %d, want 404", rec.Code)
	}
}
