//go:build !nodashboard

package server

import (
	"embed"
	"io/fs"
)

// The all: prefix keeps dot-files (the .gitkeep placeholder) in the embed so
// a fresh checkout — where web/dist has never been built — still compiles.
// `make web` copies web/dist into this directory before `go build`.
//
//go:embed all:webdist
var webdistFS embed.FS

// distFS is the dashboard bundle served at /. The nodashboard build tag
// swaps this for an empty FS (spaHandler then 404s everything).
func distFS() fs.FS {
	sub, err := fs.Sub(webdistFS, "webdist")
	if err != nil {
		panic("server: embedded webdist directory missing: " + err.Error())
	}
	return sub
}
