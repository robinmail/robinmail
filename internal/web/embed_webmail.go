//go:build webmail

package web

import (
	"embed"
	"io/fs"
)

// dist is the built webmail: "make web" copies web/dist here. The
// directory is not in git, so this file only compiles after that; without
// the "webmail" tag the binary has the placeholder page instead.
//
// "all:" because Vite names some chunks with a leading underscore, which
// a plain pattern leaves out.
//
//go:embed all:dist
var dist embed.FS

// Embedded returns the webmail built into this binary, or nil when it was
// built without one (see the Makefile).
func Embedded() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	return sub
}
