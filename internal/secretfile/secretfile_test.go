package secretfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// write replaces path with a new file (a new inode), as the kubelet does
// when it swaps a Secret volume's ..data link.
func write(t *testing.T, path, content string) {
	t.Helper()
	tmp := path + ".new"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type logs struct {
	mu   sync.Mutex
	msgs []string
}

func (l *logs) logf(_ bool, format string, args ...any) {
	l.mu.Lock()
	l.msgs = append(l.msgs, fmt.Sprintf(format, args...))
	l.mu.Unlock()
}
func (l *logs) all() string { l.mu.Lock(); defer l.mu.Unlock(); return strings.Join(l.msgs, "\n") }

func setup(t *testing.T, content string, opts ...Option) (*File, string, *clock, *logs) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	write(t, path, content)
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	lg := &logs{}
	opts = append([]Option{WithClock(clk.now), WithLogf(lg.logf)}, opts...)
	f, err := Open(path, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return f, path, clk, lg
}

func TestOpenReadsAndTrims(t *testing.T) {
	f, _, _, _ := setup(t, "  s3cret-one\n")
	if got := f.Value(); got != "s3cret-one" {
		t.Fatalf("Value = %q", got)
	}
}

func TestOpenRefusesEmptyAndMissing(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	write(t, empty, " \n")
	if _, err := Open(empty); err == nil || !strings.Contains(err.Error(), empty) {
		t.Fatalf("empty file: err = %v, want one naming the file", err)
	}
	if _, err := Open(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing file: want an error")
	}
}

func TestRotationIsSeenWithinTheCheckInterval(t *testing.T) {
	f, path, clk, lg := setup(t, "old-token")
	write(t, path, "new-token")
	if got := f.Value(); got != "old-token" {
		t.Fatalf("inside the interval Value = %q, want the old one (no stat yet)", got)
	}
	clk.advance(DefaultCheckInterval)
	if got := f.Value(); got != "new-token" {
		t.Fatalf("after the interval Value = %q, want new-token", got)
	}
	if !strings.Contains(lg.all(), path) || strings.Contains(lg.all(), "new-token") || strings.Contains(lg.all(), "old-token") {
		t.Fatalf("log must name the file and never a value, got %q", lg.all())
	}
}

func TestChecksAtMostOncePerInterval(t *testing.T) {
	f, path, clk, _ := setup(t, "a-token")
	prev := "a-token"
	for i := range 3 {
		want := fmt.Sprintf("token-%d", i)
		write(t, path, want)
		clk.advance(DefaultCheckInterval - time.Second)
		if got := f.Value(); got != prev {
			t.Fatalf("round %d: checked before the interval elapsed: Value = %q, want the old %q", i, got, prev)
		}
		clk.advance(time.Second)
		if got := f.Value(); got != want {
			t.Fatalf("round %d: Value = %q, want %q", i, got, want)
		}
		// However often it is asked, it does not look again within the
		// interval it just started: a rotation now waits for the next check.
		write(t, path, want+"-again")
		for range 5 {
			if got := f.Value(); got != want {
				t.Fatalf("round %d: looked again within the interval: Value = %q, want %q", i, got, want)
			}
		}
		prev = want
	}
}

func TestEmptyOrUnreadableFileKeepsTheOldValue(t *testing.T) {
	f, path, clk, lg := setup(t, "keep-me")

	write(t, path, "\n  \n")
	clk.advance(DefaultCheckInterval)
	if got := f.Value(); got != "keep-me" {
		t.Fatalf("empty file: Value = %q, want keep-me", got)
	}
	if !strings.Contains(lg.all(), path) {
		t.Fatalf("an empty file must be logged with its name, got %q", lg.all())
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	clk.advance(DefaultCheckInterval)
	if got := f.Value(); got != "keep-me" {
		t.Fatalf("missing file: Value = %q, want keep-me", got)
	}

	// Unreadable: a directory where the file was.
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	clk.advance(DefaultCheckInterval)
	if got := f.Value(); got != "keep-me" {
		t.Fatalf("unreadable file: Value = %q, want keep-me", got)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	// It recovers when a good file comes back.
	write(t, path, "recovered")
	clk.advance(DefaultCheckInterval)
	if got := f.Value(); got != "recovered" {
		t.Fatalf("after recovery Value = %q", got)
	}
}

func TestEachDistinctFailureIsLoggedOnce(t *testing.T) {
	f, path, clk, lg := setup(t, "keep-me")
	write(t, path, "")
	for range 5 {
		clk.advance(DefaultCheckInterval)
		f.Value()
	}
	if n := strings.Count(lg.all(), "keeping the previous value"); n != 1 {
		t.Fatalf("logged %d times, want once:\n%s", n, lg.all())
	}
}

func TestValidateRejectsACandidateAndKeepsTheOldValue(t *testing.T) {
	f, path, clk, lg := setup(t, "good-token", WithValidate(func(v string) error {
		if strings.Contains(v, " ") {
			return errors.New("has a space")
		}
		return nil
	}))
	write(t, path, "bad token")
	clk.advance(DefaultCheckInterval)
	if got := f.Value(); got != "good-token" {
		t.Fatalf("Value = %q, want good-token", got)
	}
	if strings.Contains(lg.all(), "bad token") || !strings.Contains(lg.all(), "has a space") {
		t.Fatalf("log = %q", lg.all())
	}
}

func TestOpenAppliesValidateToTheFirstValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t")
	write(t, path, "bad token")
	_, err := Open(path, WithValidate(func(string) error { return errors.New("nope") }))
	if err == nil || !strings.Contains(err.Error(), path) || strings.Contains(err.Error(), "bad token") {
		t.Fatalf("err = %v", err)
	}
}

func TestOptionalFileMayAppearLaterAndRemovalCanRevoke(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ingest")
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	f, err := Open(path, Optional(), RemovalClears(), WithClock(clk.now), WithLogf(func(bool, string, ...any) {}))
	if err != nil {
		t.Fatalf("an optional file may be missing at Open: %v", err)
	}
	if f.Value() != "" {
		t.Fatalf("Value = %q, want empty", f.Value())
	}
	write(t, path, "added-later")
	clk.advance(DefaultCheckInterval)
	if got := f.Value(); got != "added-later" {
		t.Fatalf("Value = %q", got)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	clk.advance(DefaultCheckInterval)
	if got := f.Value(); got != "" {
		t.Fatalf("a removed file with RemovalClears must revoke, Value = %q", got)
	}
	// An emptied (not removed) file still keeps the old value.
	write(t, path, "again")
	clk.advance(DefaultCheckInterval)
	if got := f.Value(); got != "again" {
		t.Fatalf("Value = %q, want again", got)
	}
	write(t, path, "")
	clk.advance(DefaultCheckInterval)
	if got := f.Value(); got != "again" {
		t.Fatalf("an empty file keeps the old value, got %q", got)
	}
}

func TestOptionalWithoutRemovalClearsKeepsTheValueWhenTheFileGoes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hook")
	write(t, path, "https://hooks.example/abc")
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	f, err := Open(path, Optional(), WithClock(clk.now), WithLogf(func(bool, string, ...any) {}))
	if err != nil {
		t.Fatal(err)
	}
	os.Remove(path)
	clk.advance(DefaultCheckInterval)
	if got := f.Value(); got != "https://hooks.example/abc" {
		t.Fatalf("Value = %q", got)
	}
}

func TestSymlinkSwapLikeAKubeletSecretVolume(t *testing.T) {
	dir := t.TempDir()
	mk := func(name, content string) {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, "token"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mk("..v1", "one-token")
	if err := os.Symlink("..v1", filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..data", "token"), filepath.Join(dir, "token")); err != nil {
		t.Fatal(err)
	}
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	f, err := Open(filepath.Join(dir, "token"), WithClock(clk.now), WithLogf(func(bool, string, ...any) {}))
	if err != nil {
		t.Fatal(err)
	}
	// Same size and the same instant: only the inode differs.
	mk("..v2", "two-token")
	if err := os.Symlink("..v2", filepath.Join(dir, "..data_tmp")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "..data_tmp"), filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	clk.advance(DefaultCheckInterval)
	if got := f.Value(); got != "two-token" {
		t.Fatalf("Value = %q, want two-token", got)
	}
}

func TestOversizedFileIsRefused(t *testing.T) {
	f, path, clk, _ := setup(t, "small")
	write(t, path, strings.Repeat("x", MaxSize+1))
	clk.advance(DefaultCheckInterval)
	if got := f.Value(); got != "small" {
		t.Fatalf("Value = %d bytes, want the old value", len(got))
	}
}

func TestConcurrentValueAndRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	write(t, path, "tok-0")
	f, err := Open(path, WithCheckInterval(0), WithLogf(func(bool, string, ...any) {}))
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if v := f.Value(); !strings.HasPrefix(v, "tok-") {
					t.Errorf("torn or empty value %q", v)
					return
				}
			}
		}()
	}
	for i := 1; i <= 50; i++ {
		write(t, path, fmt.Sprintf("tok-%d", i))
		time.Sleep(time.Millisecond)
	}
	close(stop)
	wg.Wait()
	if got := f.Value(); got != "tok-50" {
		t.Fatalf("final Value = %q", got)
	}
}

func TestOptionalFileMayBeEmptyAtOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hook")
	write(t, path, "\n")
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	f, err := Open(path, Optional(), WithClock(clk.now), WithLogf(func(bool, string, ...any) {}))
	if err != nil || f.Value() != "" {
		t.Fatalf("an empty optional file: err %v, value %q", err, f.Value())
	}
	write(t, path, "filled-in")
	clk.advance(DefaultCheckInterval)
	if got := f.Value(); got != "filled-in" {
		t.Fatalf("Value = %q", got)
	}
	if _, err := Open(filepath.Join(t.TempDir(), "x"), WithLogf(func(bool, string, ...any) {})); err == nil {
		t.Fatal("a required file must exist")
	}
	empty := filepath.Join(t.TempDir(), "empty")
	write(t, empty, "")
	if _, err := Open(empty); err == nil || err.Error() != empty+" is empty" {
		t.Fatalf("a required empty file: err = %v", err)
	}
}

// Files in one reload group never reload at the same time, and a Value that
// finds the group busy neither waits nor loses its check.
func TestReloadGroupSerializesReloadsWithoutBlocking(t *testing.T) {
	var group sync.Mutex
	inside, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	slow, slowPath, _, _ := setup(t, "slow-1", WithCheckInterval(0), WithReloadGroup(&group),
		WithValidate(func(v string) error {
			if v == "slow-2" {
				once.Do(func() { close(inside) })
				<-release
			}
			return nil
		}))
	other, otherPath, _, _ := setup(t, "other-1", WithCheckInterval(0), WithReloadGroup(&group))

	write(t, slowPath, "slow-2")
	write(t, otherPath, "other-2")
	done := make(chan string)
	go func() { done <- slow.Value() }()
	<-inside // slow is mid-reload, holding the group
	got := make(chan string)
	go func() { got <- other.Value() }()
	select {
	case v := <-got:
		if v != "other-1" {
			t.Fatalf("Value during another file's reload = %q, want the old value, unchecked", v)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Value waited for another file's reload")
	}
	close(release)
	if v := <-done; v != "slow-2" {
		t.Fatalf("slow = %q", v)
	}
	if v := other.Value(); v != "other-2" {
		t.Fatalf("the skipped check was lost: Value = %q, want other-2", v)
	}
}
