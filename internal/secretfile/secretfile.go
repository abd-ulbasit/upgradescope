// Package secretfile reads a secret from a file and picks up a new value
// when the file changes: how serve and the agent rotate a token or a webhook
// URL mounted from a Kubernetes Secret without a restart.
//
// The kubelet updates a Secret volume by writing a new directory and
// repointing a symlink, so a changed Secret is a different file behind the
// same path. A File notices that (the file's identity, modification time or
// size differs), at most once per check interval and only when something
// asks for the value, so an idle process does no work. A new value that is
// empty, unreadable or refused by the validator never replaces a good one: the
// old value stays in service and the reason is logged once, naming the file
// and never its contents.
package secretfile

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultCheckInterval is how often Value may stat the file for a change:
// at most one stat every 5 seconds, whatever the request rate.
const DefaultCheckInterval = 5 * time.Second

// MaxSize is the largest file read. A token or a webhook URL is far below
// it; a larger file is something else, and is refused rather than read.
const MaxSize = 64 << 10

// errEmpty is wrapped by the error for an empty file; its text is empty so
// the message reads "<path> is empty".
var errEmpty = emptyError{}

type emptyError struct{}

func (emptyError) Error() string { return "" }

// File is a secret that follows its file. It is safe for concurrent use.
type File struct {
	path          string
	interval      time.Duration
	now           func() time.Time
	logf          func(isError bool, format string, args ...any)
	validate      func(string) error
	optional      bool
	removalClears bool
	group         *sync.Mutex // shared by Files whose validators read each other, nil = none

	value atomic.Pointer[string] // the value in service; never nil after Open

	// check serializes the stat and read; a Value call that finds a check
	// under way returns the current value instead of waiting for it, so a
	// slow filesystem never stalls the requests behind one.
	check   sync.Mutex
	next    atomic.Int64 // unix nanoseconds before which Value does not look
	stamp   os.FileInfo  // of the file value was loaded from, nil = none, under check
	failure string       // last failure logged, "" = none since the last load, under check
}

// Option configures Open.
type Option func(*File)

// WithCheckInterval sets how often Value may stat the file; 0 checks on
// every call (tests).
func WithCheckInterval(d time.Duration) Option { return func(f *File) { f.interval = d } }

// WithClock replaces time.Now (tests).
func WithClock(now func() time.Time) Option { return func(f *File) { f.now = now } }

// WithLogf sends the reload log lines to logf instead of the standard logger.
// isError is true for a value that could not be taken or was removed, false
// for a reload that worked. A line names the file and never a value.
func WithLogf(logf func(isError bool, format string, args ...any)) Option {
	return func(f *File) { f.logf = logf }
}

// stdLogf is the default: the standard logger, errors marked.
func stdLogf(isError bool, format string, args ...any) {
	if isError {
		format = "ERROR " + format
	}
	log.Printf(format, args...)
}

// WithReloadGroup serializes this File's reloads with those of every other File
// given the same mutex, from the validator's comparison to the swap. For
// Files whose validators compare a candidate with another File's value (the
// server's three tokens must stay different): two reloading at once would
// each pass against the other's old value and leave two equal ones. A Value
// that finds the group busy returns the current value and looks again on its
// next call, so a request never waits on another file's reload.
func WithReloadGroup(mu *sync.Mutex) Option { return func(f *File) { f.group = mu } }

// WithValidate refuses a value validate returns an error for: the first
// value fails Open, a later one is logged and not taken. Its error is
// logged, so it must not repeat the value.
func WithValidate(validate func(string) error) Option {
	return func(f *File) { f.validate = validate }
}

// Optional lets the file be missing or empty at Open (a Secret key that is
// absent or empty): the value is "" until it has content.
func Optional() Option { return func(f *File) { f.optional = true } }

// RemovalClears makes a file that was removed (not emptied: an empty file
// keeps the old value) clear the value to "". For a credential whose
// removal is the way to revoke it, so that deleting the key from the Secret
// stops the old token working instead of leaving it in service until a
// restart.
func RemovalClears() Option { return func(f *File) { f.removalClears = true } }

// Open reads path now, so a missing, empty or unacceptable file fails the
// start instead of the first request (an Optional file may be missing).
func Open(path string, opts ...Option) (*File, error) {
	f := &File{path: path, interval: DefaultCheckInterval, now: time.Now, logf: stdLogf}
	for _, o := range opts {
		o(f)
	}
	f.next.Store(f.now().Add(f.interval).UnixNano())
	empty := ""
	f.value.Store(&empty)
	st, err := os.Stat(path)
	if err != nil {
		if f.optional && errors.Is(err, fs.ErrNotExist) {
			return f, nil
		}
		return nil, err
	}
	v, err := f.read()
	if err != nil && !(f.optional && errors.Is(err, errEmpty)) {
		return nil, err
	}
	f.value.Store(&v)
	f.stamp = st
	return f, nil
}

// Current is the value in service without a look at the file for a change.
func (f *File) Current() string { return *f.value.Load() }

// Path is the file's path.
func (f *File) Path() string { return f.path }

// Value is the secret now: the file's current contents, trimmed of
// surrounding whitespace, as of the last check.
func (f *File) Value() string {
	if now := f.now(); now.UnixNano() >= f.next.Load() && f.check.TryLock() {
		defer f.check.Unlock()
		if now.UnixNano() >= f.next.Load() {
			if f.group != nil {
				if !f.group.TryLock() {
					return *f.value.Load() // another file is reloading: look again on the next call
				}
				defer f.group.Unlock()
			}
			f.next.Store(now.Add(f.interval).UnixNano())
			f.reloadIfChanged()
		}
	}
	return *f.value.Load()
}

func (f *File) reloadIfChanged() {
	st, err := os.Stat(f.path) // follows symlinks: a Secret volume's files are links through ..data
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) && f.removalClears && f.stamp != nil {
			f.stamp = nil
			empty := ""
			f.value.Store(&empty)
			f.failure = ""
			f.logf(true, "secretfile: %s was removed: its value is no longer in service", f.path)
			return
		}
		f.fail(err)
		return
	}
	if f.stamp != nil && sameFile(st, f.stamp) {
		return
	}
	v, err := f.read()
	if err != nil {
		f.fail(err)
		return
	}
	f.stamp, f.failure = st, ""
	f.value.Store(&v)
	f.logf(false, "secretfile: %s reloaded", f.path)
}

// fail keeps the value in service and logs why, once per distinct reason.
func (f *File) fail(err error) {
	if msg := err.Error(); msg != f.failure {
		f.failure = msg
		f.logf(true, "secretfile: %v; keeping the previous value", err)
	}
}

// read returns the file's trimmed contents, refusing an empty file, an
// oversized one and a value the validator refuses. Errors never carry the
// contents.
func (f *File) read() (string, error) {
	fh, err := os.Open(f.path)
	if err != nil {
		return "", err
	}
	defer fh.Close()
	raw, err := io.ReadAll(io.LimitReader(fh, MaxSize+1))
	if err != nil {
		return "", err
	}
	if len(raw) > MaxSize {
		return "", fmt.Errorf("%s is larger than %d bytes", f.path, MaxSize)
	}
	v := strings.TrimSpace(string(raw))
	if v == "" {
		return "", fmt.Errorf("%s is empty%w", f.path, errEmpty)
	}
	if f.validate != nil {
		if err := f.validate(v); err != nil {
			return "", fmt.Errorf("%s: %w", f.path, err)
		}
	}
	return v, nil
}

// sameFile reports whether b is a's file, unchanged: the same file (a
// replaced one is another), modification time and size.
func sameFile(a, b os.FileInfo) bool {
	return os.SameFile(a, b) && a.ModTime().Equal(b.ModTime()) && a.Size() == b.Size()
}
