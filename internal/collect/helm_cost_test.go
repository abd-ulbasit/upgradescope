package collect

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// TestHelmDecodeCostByRelease measures the CPU one release costs to decode
// (decodeHelmEntry: the gzip and JSON of the stored release, then its
// manifest parsed for the flagged APIs), release by release in the order
// the Helm step visits them (namespace, then name), and says how the cost
// is spread along that order (#247).
//
// It is a diagnostic, not a check, and runs only when
// UPGRADESCOPE_HELM_PAYLOADS names a file of tab-separated lines
// "namespace, name, base64 of the Secret's data.release", which a lab
// yields with
//
//	kubectl get secrets -A -l owner=helm -o json | jq -r '.items[] |
//	  [.metadata.namespace, .metadata.name, .data.release] | @tsv'
//
// Why it exists: the tick after a partial Helm step decodes only the
// releases the partial step left unread, which are the LAST ones in that
// order, and a mean cost per release does not predict them. The bench fill
// (hack/bench/seed) names its releases so that the last 5% in that order are
// exactly its large releases (300 objects), about 8 times the mean release's CPU;
// a cost of "the mean times the count" came out 5 CPU-seconds short on a pod
// at 200m, and nothing was decoded twice.
func TestHelmDecodeCostByRelease(t *testing.T) {
	path := os.Getenv("UPGRADESCOPE_HELM_PAYLOADS")
	if path == "" {
		t.Skip("diagnostic: set UPGRADESCOPE_HELM_PAYLOADS to a file of namespace, name and base64 payload lines (see the test's comment)")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	type release struct {
		key     string
		payload []byte
	}
	var rels []release
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<26)
	for sc.Scan() {
		p := strings.Split(sc.Text(), "\t")
		if len(p) != 3 {
			t.Fatalf("line %q: want namespace, name and payload separated by tabs", sc.Text())
		}
		b, err := base64.StdEncoding.DecodeString(p[2])
		if err != nil {
			t.Fatalf("%s/%s: %v", p[0], p[1], err)
		}
		rels = append(rels, release{p[0] + "/" + p[1], b})
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(rels) == 0 {
		t.Fatal("no releases in the file")
	}
	slices.SortFunc(rels, func(a, b release) int { return strings.Compare(a.key, b.key) })

	k, err := kb.Load()
	if err != nil {
		t.Fatal(err)
	}
	flagged := map[gvk]bool{}
	for _, e := range k.APILifecycle {
		if e.Deprecated != nil || e.Removed != nil {
			flagged[gvk{e.Group, e.Version, e.Kind}] = true
		}
	}
	const reps = 3
	cost := make([]time.Duration, len(rels))
	for range reps {
		for i, r := range rels {
			before := processCPU()
			decodeHelmEntry(r.payload, flagged)
			cost[i] += processCPU() - before
		}
	}
	var total time.Duration
	for i := range cost {
		cost[i] /= reps
		total += cost[i]
	}
	if total == 0 {
		t.Skip("this platform reports no process CPU time")
	}
	t.Logf("%d releases: %v of CPU to decode them all (mean %v each), the mean of %d runs", len(rels), total.Round(time.Millisecond), (total / time.Duration(len(rels))).Round(10*time.Microsecond), reps)
	// The share of the cost in each twentieth of the order, and the cost of
	// the last releases against what their count at the mean would be.
	const bands = 20
	var line []string
	for b := range bands {
		lo, hi := b*len(rels)/bands, (b+1)*len(rels)/bands
		var c time.Duration
		for _, d := range cost[lo:hi] {
			c += d
		}
		line = append(line, fmt.Sprintf("%.0f%%", 100*float64(c)/float64(total)))
	}
	t.Logf("share of the CPU by twentieth of the order the step visits them in: %s", strings.Join(line, " "))
	// The tails a partial step leaves unread: 5% of the releases, and the
	// counts in UPGRADESCOPE_HELM_TAIL (comma separated, e.g. "43,51").
	tails := []int{len(rels) / 20}
	for _, f := range strings.Split(os.Getenv("UPGRADESCOPE_HELM_TAIL"), ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(f)); err == nil && n > 0 && n <= len(rels) {
			tails = append(tails, n)
		}
	}
	for _, n := range tails {
		var c time.Duration
		for _, d := range cost[len(cost)-n:] {
			c += d
		}
		t.Logf("the last %d releases: %v of CPU, %.1fx what %d at the mean would cost (the mean says %v)",
			n, c.Round(time.Millisecond), float64(c)/(float64(total)*float64(n)/float64(len(rels))), n, (total * time.Duration(n) / time.Duration(len(rels))).Round(time.Millisecond))
	}
}
