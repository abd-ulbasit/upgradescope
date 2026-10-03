package apigroup

import "testing"

// The pre-v0.2.0 group is spelled out here on purpose: these tests pin
// what objects annotated by v0.1.x and the v0.2.0 release candidates
// carry.
func TestLegacyConstants(t *testing.T) {
	for _, c := range []struct{ got, want string }{
		{LegacyGroup, "upgradescope.dev"},
		{LegacyIgnoreAnnotation, "upgradescope.dev/ignore"},
		{LegacyIgnoreReasonAnnotation, "upgradescope.dev/ignore-reason"},
		{Group, "upgradescope.basit.engineer"},
		{IgnoreAnnotation, "upgradescope.basit.engineer/ignore"},
		{IgnoreReasonAnnotation, "upgradescope.basit.engineer/ignore-reason"},
		{StatusErrorAnnotation, "upgradescope.basit.engineer/status-error"},
	} {
		if c.got != c.want {
			t.Errorf("constant = %q, want %q", c.got, c.want)
		}
	}
}

func TestReadIgnore(t *testing.T) {
	cases := []struct {
		name           string
		ann            map[string]string
		ignore, reason string
		legacy         bool
	}{
		{name: "none", ann: map[string]string{"other": "x"}},
		{name: "current keys",
			ann:    map[string]string{IgnoreAnnotation: "removed-api", IgnoreReasonAnnotation: "gone"},
			ignore: "removed-api", reason: "gone"},
		{name: "old keys are still honoured, and say so",
			ann:    map[string]string{"upgradescope.dev/ignore": "removed-api", "upgradescope.dev/ignore-reason": "gone"},
			ignore: "removed-api", reason: "gone", legacy: true},
		{name: "the current key wins when both are set",
			ann: map[string]string{
				IgnoreAnnotation: "removed-api", IgnoreReasonAnnotation: "new reason",
				"upgradescope.dev/ignore": "eol-addon", "upgradescope.dev/ignore-reason": "old reason",
			},
			ignore: "removed-api", reason: "new reason"},
		{name: "a current key set empty still wins",
			ann:    map[string]string{IgnoreAnnotation: "", "upgradescope.dev/ignore": "removed-api"},
			ignore: ""},
		{name: "mixed: the old reason fills in, and is reported",
			ann:    map[string]string{IgnoreAnnotation: "removed-api", "upgradescope.dev/ignore-reason": "gone"},
			ignore: "removed-api", reason: "gone", legacy: true},
		{name: "an old reason alone is read and reported",
			ann:    map[string]string{"upgradescope.dev/ignore-reason": "gone"},
			reason: "gone", legacy: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ignore, reason, legacy := ReadIgnore(func(k string) (string, bool) { v, ok := c.ann[k]; return v, ok })
			if ignore != c.ignore || reason != c.reason || legacy != c.legacy {
				t.Errorf("ReadIgnore = (%q, %q, %v), want (%q, %q, %v)", ignore, reason, legacy, c.ignore, c.reason, c.legacy)
			}
		})
	}
}

func TestLegacyIgnoreWarning(t *testing.T) {
	const want = "object shop/web: annotation keys upgradescope.dev/ignore and upgradescope.dev/ignore-reason are deprecated and read only until v0.3.0: rename them to upgradescope.basit.engineer/ignore and upgradescope.basit.engineer/ignore-reason"
	if got := LegacyIgnoreWarning("shop/web"); got != want {
		t.Errorf("LegacyIgnoreWarning =\n%s\nwant\n%s", got, want)
	}
}
