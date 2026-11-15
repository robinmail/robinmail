//go:build webmail

package web

import (
	"embed"
	"io/fs"
)

// dist is the built webmail (web-ng, Angular): "make" copies
// web-ng/dist/shell here. The directory is not in git, so this file only
// compiles after that; without the "webmail" tag the binary has the
// placeholder page instead. index.html and the files named by their
// content under assets/, which the handler caches for a year.
//
//go:embed all:dist
var dist embed.FS

// Embedded returns the webmail built into this binary.
func Embedded() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	return sub
}
