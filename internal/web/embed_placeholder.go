//go:build !webmail && !webmail_ng

package web

import "io/fs"

// Embedded returns the webmail built into this binary, or nil when it was
// built without one: a plain "go build" needs no Node and serves the
// placeholder page. "make" builds the webmail and sets the "webmail" tag;
// "make build-ng" builds the Angular one and sets "webmail_ng".
func Embedded() fs.FS { return nil }
