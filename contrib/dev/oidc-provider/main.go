// Command oidc-provider is an OpenID Connect provider for development and
// the browser tests: the fake one of internal/oidc/oidctest on a fixed
// address. It logs in one fixed user without asking anything, so it is
// good for trying the "Log in with ..." button and for nothing else.
//
//	go run ./contrib/dev/oidc-provider -listen 127.0.0.1:8090 -email dev@robinmail.test
//
// "make e2e" starts it and gives the instance it tests this provider:
//
//	"auth": {"oidc": [{"name": "test", "label": "Test provider", "issuer": "http://127.0.0.1:8090",
//	                   "client_id": "robinmail", "client_secret": "test-secret-of-the-fake-provider"}]}
//
// The client secret is the constant of oidctest: a provider that asks no
// password has no secret worth the name.
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/robinmail/robinmail/internal/oidc/oidctest"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8090", "address to listen on; loopback only")
	email := flag.String("email", "dev@robinmail.test", "the email claim of the user who is logged in")
	subject := flag.String("sub", "dev-user-1", "the sub claim")
	flag.Parse()
	p := oidctest.NewAt(*listen)
	defer p.Close()
	p.User = map[string]any{"sub": *subject, "email": *email, "email_verified": true}
	fmt.Fprintf(os.Stderr, "fake OpenID provider on %s: client %s, logs in %s without asking\n", p.Issuer, p.ClientID, *email)
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
}
