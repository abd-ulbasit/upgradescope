//go:build nodashboard

package server

import "io/fs"

// distFS under -tags nodashboard: no embedded bundle at all (smallest
// binary, no node toolchain needed). spaHandler serves a JSON 404 for
// every non-API path.
func distFS() fs.FS { return emptyFS{} }

type emptyFS struct{}

func (emptyFS) Open(name string) (fs.File, error) {
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}
