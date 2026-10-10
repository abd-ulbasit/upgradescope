package agent

import (
	"context"
	"log/slog"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// #238: push settings that can never work are refused at startup instead
// of failing every push as if the server were down.
func TestValidateServerURL(t *testing.T) {
	for _, bad := range []string{
		"upgradescope.example.com", // no scheme: "unsupported protocol scheme" on every push
		"localhost:8080",           // parses as scheme "localhost"
		"ftp://upgradescope.example.com",
		"http://",
		"https:///api",
		"://x",
		"https://:8080", // a port, no host: Go would dial localhost
	} {
		if err := ValidateServerURL(bad); err == nil {
			t.Errorf("ValidateServerURL(%q) = nil, want an error", bad)
		}
	}
	for _, good := range []string{"http://scope:8080", "https://upgradescope.example.com", "HTTPS://hub.internal/base/", "http://127.0.0.1:8080"} {
		if err := ValidateServerURL(good); err != nil {
			t.Errorf("ValidateServerURL(%q) = %v, want nil", good, err)
		}
	}
}

func TestValidateServerToken(t *testing.T) {
	for _, bad := range []string{"abc\n", " abc", "a b", "a\tb", "a\x00b", "a\x7fb"} {
		if err := ValidateServerToken(bad); err == nil {
			t.Errorf("ValidateServerToken(%q) = nil, want an error", bad)
		}
	}
	if err := ValidateServerToken("usc_0123456789abcdef"); err != nil {
		t.Errorf("a plain token refused: %v", err)
	}
}

func TestConfigRefusesUnusablePushSettings(t *testing.T) {
	for _, cfg := range []Config{
		{ServerURL: "upgradescope.example.com", ServerToken: "t"},
		{ServerURL: "https://upgradescope.example.com", ServerToken: "t\n"},
		{ForceSyncEvery: -time.Second},
	} {
		if err := cfg.applyDefaults(); err == nil {
			t.Errorf("applyDefaults(%+v) = nil, want an error", cfg)
		}
	}
}

// 0 is not "push every tick" nor silently the 1h default for a value given
// explicitly (the flag): it is refused, like a negative period.
func TestValidateForceSyncEvery(t *testing.T) {
	for _, d := range []time.Duration{0, -time.Second} {
		if err := ValidateForceSyncEvery(d); err == nil {
			t.Errorf("ValidateForceSyncEvery(%v) = nil, want an error", d)
		}
	}
	if err := ValidateForceSyncEvery(time.Minute); err != nil {
		t.Errorf("ValidateForceSyncEvery(1m) = %v", err)
	}
}

// A period at or below the interval means "every tick", as it did before #238:
// Run lowers it to the shortest spacing two ticks can have (the jitter
// floor, 9/10 of the interval) and says so. Raising it to the interval
// instead skipped the force-sync on every tick the jitter brought early.
func TestRunForceSyncBelowTheIntervalMeansEveryTick(t *testing.T) {
	for _, tc := range []struct{ forceSync, inEffect string }{{"30s", "4m30s"}, {"5m0s", "4m30s"}} {
		logs := &syncBuffer{}
		srv := newSnapServer(t)
		fs, err := time.ParseDuration(tc.forceSync)
		if err != nil {
			t.Fatal(err)
		}
		cfg := Config{ServerURL: srv.srv.URL, ServerToken: "t", ForceSyncEvery: fs, Interval: 5 * time.Minute,
			Logger: slog.New(slog.NewJSONHandler(logs, nil))}
		runOneTick(t, fakeAPIExt(), cfg)
		var warned bool
		for _, l := range logs.lines(t) {
			if l["level"] == "WARN" && strings.Contains(l["msg"].(string), "every tick") &&
				l["forceSyncEvery"] == tc.forceSync && l["interval"] == "5m0s" && l["inEffect"] == tc.inEffect {
				warned = true
			}
		}
		if !warned {
			t.Errorf("no WARN line saying a %s force-sync-every means every 5m tick (%s in effect): %v", tc.forceSync, tc.inEffect, logs.lines(t))
		}
	}
	for _, tc := range []struct{ forceSync, interval, want time.Duration }{
		{30 * time.Second, 5 * time.Minute, 4*time.Minute + 30*time.Second},
		{time.Minute, 10 * time.Minute, 9 * time.Minute},
		{9*time.Minute + 59*time.Second, 10 * time.Minute, 9 * time.Minute},            // below the interval: every tick, even above the floor
		{10 * time.Minute, 10 * time.Minute, 9 * time.Minute},                          // at the interval: every tick too
		{10*time.Minute + time.Second, 10 * time.Minute, 10*time.Minute + time.Second}, // above: kept
		{time.Hour, 10 * time.Minute, time.Hour},
	} {
		if got := forceSyncInEffect(tc.forceSync, tc.interval); got != tc.want {
			t.Errorf("forceSyncInEffect(%v, %v) = %v, want %v", tc.forceSync, tc.interval, got, tc.want)
		}
	}
}

// A --pod-pass-max-age that is not above (pod-pass-every minus one) times the
// longest tick spacing, 11/10 of the interval, cannot let a pass be reused
// for every one of those ticks whatever the jitter draws: at or below the
// interval none is reused but by a tick the jitter brings early, and above
// it the last reuses fail on ticks the jitter spaces widely. Run warns once
// at the start, naming the settings and the floor, and only when a pass
// could have been reused otherwise (#228).
func TestRunWarnsWhenPodPassMaxAgeIsTooShortForPodPassEvery(t *testing.T) {
	for _, tc := range []struct {
		name   string
		every  int
		maxAge time.Duration
		warn   bool
		floor  string
		fits   float64 // the largest --pod-pass-every whose floor is under maxAge
	}{
		{"below the interval", 3, time.Minute, true, "11m0s", 1},
		{"at the interval", 3, 5 * time.Minute, true, "11m0s", 1},
		{"above the interval, in the jitter's reach of one reuse", 3, 5*time.Minute + time.Second, true, "11m0s", 1},
		{"twice the interval, short of two long spacings", 3, 10 * time.Minute, true, "11m0s", 2},
		{"at the floor of two reuses", 3, 11 * time.Minute, true, "11m0s", 2},
		{"above the floor of two reuses", 3, 11*time.Minute + time.Second, false, "", 0},
		{"every 2 needs one long spacing, not two", 2, 5*time.Minute + 30*time.Second, true, "5m30s", 1},
		{"every 2 above its floor", 2, 5*time.Minute + 31*time.Second, false, "", 0},
		{"every 4 needs three long spacings", 4, 16*time.Minute + 30*time.Second, true, "16m30s", 3},
		{"every 1 lists every tick anyway", 1, time.Minute, false, "", 0},
		{"defaults", 0, 0, false, "", 0},
		// The 1<<40 and 1<<50 constants in this file assume a 64-bit int (the
		// release targets are amd64 and arm64); a 32-bit target would need them
		// narrowed.
		// 1<<40 spacings of 5m30s overflow a Duration (about 292 years):
		// the floor saturates at the largest one instead of wrapping to a
		// small or negative value that would hide the warning.
		{"a huge every saturates the floor", 1 << 40, 1000 * time.Hour, true, "2562047h47m16.854775807s", 10910},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := &syncBuffer{}
			cfg := Config{Interval: 5 * time.Minute, PodPassEvery: tc.every, PodPassMaxAge: tc.maxAge,
				Logger: slog.New(slog.NewJSONHandler(logs, nil))}
			runOneTick(t, fakeAPIExt(), cfg)
			var warned int
			for _, l := range logs.lines(t) {
				if l["level"] == "WARN" && strings.Contains(l["msg"].(string), "pod-pass-max-age") {
					warned++
					if l["podPassMaxAge"] != tc.maxAge.String() || l["interval"] != "5m0s" || l["podPassEvery"] != float64(tc.every) || l["mustExceed"] != tc.floor {
						t.Errorf("warning does not name the settings and the floor %q: %v", tc.floor, l)
					}
					if l["podPassEveryThatFits"] != tc.fits {
						t.Errorf("warning names podPassEveryThatFits %v, want %v: %v", l["podPassEveryThatFits"], tc.fits, l)
					}
					if !strings.Contains(l["msg"].(string), "lower pod-pass-every") {
						t.Errorf("warning does not offer lowering pod-pass-every: %v", l["msg"])
					}
				}
			}
			if want := map[bool]int{true: 1, false: 0}[tc.warn]; warned != want {
				t.Errorf("%d pod-pass-max-age warnings, want %d: %v", warned, want, logs.lines(t))
			}
		})
	}
}

// The floor is (every-1) times the longest spacing the jitter draws, 11/10
// of the interval, pinned here as literals: a formula recomputed from the
// code under test would follow it wherever it went. The interval of 7m and
// 3ns checks the integer arithmetic (the spacing is 7m42s3ns, not a rounded
// 1.1 times).
func TestPodPassMaxAgeFloorFollowsTheJitterBound(t *testing.T) {
	for _, tc := range []struct {
		interval time.Duration
		every    int
		want     time.Duration
	}{
		{time.Minute, 2, 66 * time.Second},
		{10 * time.Minute, 3, 22 * time.Minute},
		{30 * time.Minute, 2, 33 * time.Minute}, // the lowered setting troubleshooting.md offers
		{30 * time.Minute, 3, 66 * time.Minute},
		{time.Hour, 3, 132 * time.Minute},
		{7*time.Minute + 3*time.Nanosecond, 6, 38*time.Minute + 30*time.Second + 15*time.Nanosecond},
		{10 * time.Minute, 1, 0}, // nothing to reuse, nothing to bound
		{10 * time.Minute, 0, 0},
	} {
		if got := podPassMaxAgeFloor(tc.interval, tc.every); got != tc.want {
			t.Errorf("podPassMaxAgeFloor(%v, %d) = %v, want %v", tc.interval, tc.every, got, tc.want)
		}
	}
	if got, want := (7*time.Minute + 3*time.Nanosecond).String(), "7m0.000000003s"; got != want {
		t.Fatalf("test constant %s, want %s", got, want)
	}
}

// A very large --pod-pass-every must not overflow the floor into a small or
// negative duration: it saturates at the largest Duration, which no
// --pod-pass-max-age exceeds, so the warning is raised.
func TestPodPassMaxAgeFloorSaturates(t *testing.T) {
	const max = time.Duration(math.MaxInt64)
	for _, tc := range []struct {
		interval time.Duration
		every    int
	}{
		{10 * time.Minute, math.MaxInt},
		{10 * time.Minute, 1 << 40},
		{time.Minute, 1 << 50},
		{24 * time.Hour, 1_000_000_000},
	} {
		if got := podPassMaxAgeFloor(tc.interval, tc.every); got != max {
			t.Errorf("podPassMaxAgeFloor(%v, %d) = %v, want the saturated %v", tc.interval, tc.every, got, max)
		}
		if !podPassMaxAgeTooShort(Config{Interval: tc.interval, PodPassEvery: tc.every, PodPassMaxAge: max}) {
			t.Errorf("every %d at %v: even the largest max age is not above the floor, yet no warning", tc.every, tc.interval)
		}
	}
	// Just under the overflow point the product is still exact.
	every := int(math.MaxInt64 / int64(11*time.Minute)) // 11m = 1.1 x a 10m interval
	if got, want := podPassMaxAgeFloor(10*time.Minute, every+1), time.Duration(every)*11*time.Minute; got != want {
		t.Errorf("below the overflow point: %v, want exact %v", got, want)
	}
}

// The setting the warning offers: the largest --pod-pass-every whose floor
// is under the max age, at least 1.
func TestPodPassEveryThatFits(t *testing.T) {
	for _, tc := range []struct {
		interval, maxAge time.Duration
		want             int
	}{
		{30 * time.Minute, time.Hour, 2},          // floor 33m; 3 would need 66m
		{30 * time.Minute, 66 * time.Minute, 2},   // at the floor of 3 is not above it
		{30 * time.Minute, 66*time.Minute + 1, 3}, // one ns above it
		{10 * time.Minute, time.Hour, 6},          // floor of 6 is 55m; 7 needs 66m
		{10 * time.Minute, 11 * time.Minute, 1},   // at one spacing: nothing fits but 1
		{10 * time.Minute, 11*time.Minute + 1, 2}, // 2 needs only 11m
		{time.Hour, time.Hour, 1},                 // an hour interval, an hour max age
		{10 * time.Minute, time.Duration(math.MaxInt64), 1 + int(math.MaxInt64/int64(11*time.Minute))},
	} {
		if got := podPassEveryThatFits(tc.interval, tc.maxAge); got != tc.want {
			t.Errorf("podPassEveryThatFits(%v, %v) = %d, want %d", tc.interval, tc.maxAge, got, tc.want)
		}
	}
}

// The reviewer's case: --force-sync-every 1m with --interval 10m pushed an
// unchanged inventory on every tick before #238, and must still when the
// jitter brings a tick early (0.95 × the interval, and the floor itself).
// A period equal to the interval (10m/10m) means every tick as well.
func TestForceSyncBelowTheIntervalPushesEveryEarlyTick(t *testing.T) {
	for _, asked := range []time.Duration{time.Minute, 10 * time.Minute} {
		t.Run(asked.String(), func(t *testing.T) {
			srv := newSnapServer(t)
			r := testRunner(t, fakeDyn(), srv.srv.URL)
			r.cfg.Interval = 10 * time.Minute
			r.cfg.ForceSyncEvery = forceSyncInEffect(asked, r.cfg.Interval)
			cur := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
			r.now = func() time.Time { return cur }
			for i, gap := range []time.Duration{0, r.cfg.Interval * 95 / 100, minTickSpacing(r.cfg.Interval), r.cfg.Interval * 11 / 10} {
				cur = cur.Add(gap)
				if err := r.tick(context.Background()); err != nil {
					t.Fatal(err)
				}
				if srv.count() != i+1 {
					t.Fatalf("after a tick %v after the last: %d pushes, want %d (an unchanged inventory pushed every tick)", gap, srv.count(), i+1)
				}
			}
		})
	}
}

// The jitter never brings a tick closer than minTickSpacing, the floor
// forceSyncInEffect relies on, nor further than 11/10 of the interval.
func TestJitterFloorIsMinTickSpacing(t *testing.T) {
	for _, d := range []time.Duration{time.Minute, 10 * time.Minute, 7*time.Minute + 3*time.Nanosecond} {
		if got := jitterBy(d, 0); got != minTickSpacing(d) {
			t.Errorf("jitterBy(%v, 0) = %v, want the floor %v", d, got, minTickSpacing(d))
		}
		if got, hi := jitterBy(d, int64(d/5)), d+d/10; got > hi {
			t.Errorf("jitterBy(%v, max) = %v, above %v", d, got, hi)
		}
		for range 200 {
			if got := jitter(d); got < minTickSpacing(d) || got > d+d/10 {
				t.Fatalf("jitter(%v) = %v, outside [%v, %v]", d, got, minTickSpacing(d), d+d/10)
			}
		}
	}
}

// The pusher refuses, as a permanent failure that is not retried, a URL
// or token no request could carry: the startup checks are bypassed by a
// library caller that builds a pusher itself.
func TestFlushUnusableURLOrTokenIsPermanent(t *testing.T) {
	srv := newSnapServer(t)
	for _, tc := range []struct{ url, token string }{
		{"upgradescope.example.com", "t"},
		{srv.srv.URL, "abc\n"},
	} {
		p := newPusher(tc.url, tc.token, nil)
		var waits []time.Duration
		p.wait = func(_ context.Context, d time.Duration) error {
			waits = append(waits, d)
			return nil
		}
		p.offer(testPayload("c"))
		if err := p.flush(context.Background()); err == nil {
			t.Errorf("flush to %q with %q: nil error", tc.url, tc.token)
		}
		if len(waits) != 0 {
			t.Errorf("flush to %q with %q waited %v, want no retries", tc.url, tc.token, waits)
		}
	}
	if srv.count() != 0 {
		t.Errorf("%d requests reached the server", srv.count())
	}
}

// No --cluster-name and no cluster UID (the kube-system namespace could
// not be read): the server refuses a snapshot without a name, so the push
// is skipped, with an error that says why, and offered again next tick.
func TestTickSkipsThePushWithoutAClusterName(t *testing.T) {
	srv := newSnapServer(t)
	r := testRunner(t, fakeDyn(), srv.srv.URL)
	collect := r.collectFn
	r.collectFn = func(ctx context.Context) inventory.Inventory {
		inv := collect(ctx)
		inv.ClusterID = ""
		return inv
	}
	if err := r.tick(context.Background()); err == nil {
		t.Fatal("tick: want the skipped push reported")
	}
	if r.last.err != nil {
		t.Errorf("tick err = %v, want only the push failed", r.last.err)
	}
	if r.last.push != pushFailed || r.last.pushErr == nil || !strings.Contains(r.last.pushErr.Error(), "--cluster-name") {
		t.Errorf("push = %s, %v; want failed, naming --cluster-name", r.last.push, r.last.pushErr)
	}
	if srv.count() != 0 {
		t.Errorf("%d pushes, want none without a cluster name", srv.count())
	}
}
