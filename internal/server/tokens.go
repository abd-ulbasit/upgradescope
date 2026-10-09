package server

import (
	"errors"
	"fmt"

	"github.com/abd-ulbasit/upgradescope/internal/secretfile"
)

// tokenSource is one configured token: a fixed string (Config.ReadToken and
// its siblings), or a secretfile.File that follows a mounted Secret.
type tokenSource struct {
	fixed string
	file  *secretfile.File
}

func (t tokenSource) get() string {
	if t.file != nil {
		return t.file.Value()
	}
	return t.fixed
}

// current is the token in service without looking at the file for a change:
// what a validator compares a candidate with, from inside another token's
// reload.
func (t tokenSource) current() string {
	if t.file != nil {
		return t.file.Current()
	}
	return t.fixed
}

// tokenSources are the server's three bearer tokens as the requests that
// check them see them: always the current value, so a rotated Secret takes
// effect with no restart.
type tokenSources struct {
	in, rd, ad tokenSource
}

func (t *tokenSources) ingest() string { return t.in.get() }
func (t *tokenSources) read() string   { return t.rd.get() }
func (t *tokenSources) admin() string  { return t.ad.get() }

// The messages hold no token: a refused rotation logs one.
var (
	errReadIsIngest  = errors.New("Config.ReadToken must differ from Config.IngestToken: every agent's push token would read the whole fleet, and every reader could push as any cluster")
	errAdminIsRead   = errors.New("Config.AdminToken must differ from Config.ReadToken: whoever reads must not be able to delete or rename clusters")
	errAdminIsIngest = errors.New("Config.AdminToken must differ from Config.IngestToken: whoever pushes must not be able to delete or rename clusters")
)

// newTokenSources opens the token files of cfg and checks that no two of the
// three tokens are equal. cfg's token strings are left as given; a file
// replaces its string.
func newTokenSources(cfg *Config) (tokenSources, error) {
	ts := tokenSources{
		in: tokenSource{fixed: cfg.IngestToken},
		rd: tokenSource{fixed: cfg.ReadToken},
		ad: tokenSource{fixed: cfg.AdminToken},
	}
	// A candidate token for one file must not equal either of the others in
	// service now. The validators run on every reload, so a rotation cannot
	// produce a configuration New would have refused.
	open := func(path string, bad func(v string) error, extra ...secretfile.Option) (*secretfile.File, error) {
		opts := append([]secretfile.Option{secretfile.WithValidate(bad)}, extra...)
		opts = append(opts, cfg.secretOpts...)
		return secretfile.Open(path, opts...)
	}
	var err error
	if cfg.IngestTokenFile != "" {
		extra := []secretfile.Option{secretfile.RemovalClears()}
		if cfg.IngestTokenFileOptional {
			extra = append(extra, secretfile.Optional())
		}
		ts.in.file, err = open(cfg.IngestTokenFile, func(v string) error {
			if v == ts.rd.current() {
				return errReadIsIngest
			}
			if v == ts.ad.current() {
				return errAdminIsIngest
			}
			return nil
		}, extra...)
		if err != nil {
			return ts, fmt.Errorf("server: ingest token file: %w", err)
		}
	}
	if cfg.ReadTokenFile != "" {
		ts.rd.file, err = open(cfg.ReadTokenFile, func(v string) error {
			if v == ts.in.current() {
				return errReadIsIngest
			}
			if v == ts.ad.current() {
				return errAdminIsRead
			}
			return nil
		})
		if err != nil {
			return ts, fmt.Errorf("server: read token file: %w", err)
		}
	}
	if cfg.AdminTokenFile != "" {
		ts.ad.file, err = open(cfg.AdminTokenFile, func(v string) error {
			if v == ts.in.current() {
				return errAdminIsIngest
			}
			if v == ts.rd.current() {
				return errAdminIsRead
			}
			return nil
		})
		if err != nil {
			return ts, fmt.Errorf("server: admin token file: %w", err)
		}
	}
	if r, i := ts.rd.current(), ts.in.current(); r != "" && r == i {
		return ts, errReadIsIngest
	}
	if a := ts.ad.current(); a != "" && a == ts.rd.current() {
		return ts, errAdminIsRead
	}
	if a := ts.ad.current(); a != "" && a == ts.in.current() {
		return ts, errAdminIsIngest
	}
	return ts, nil
}
