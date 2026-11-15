//go:build !webmail

package web

import "io/fs"

// Embedded returns the webmail built into this binary, or nil when it was
// built without one: a plain "go build" needs no Node and serves the
// placeholder page. "make" builds the webmail and sets the "webmail" tag.
func Embedded() fs.FS { return nil }
