//go:build webmail_ng

package web

import (
	"embed"
	"io/fs"
)

// distNG is the built Angular webmail (web-ng): "make build-ng" copies
// web-ng/dist/shell here. Like dist it is not in git, so this file only
// compiles after that. Its layout is that of the Vue build, index.html and
// the files named by their content under assets/, so the handler serves it
// with the same cache headers and the same Content-Security-Policy. With
// this tag the binary has this webmail, also when "webmail" is set too.
//
//go:embed all:dist-ng
var distNG embed.FS

// Embedded returns the webmail built into this binary: here the Angular one.
func Embedded() fs.FS {
	sub, err := fs.Sub(distNG, "dist-ng")
	if err != nil {
		panic(err)
	}
	return sub
}
