package inventory

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestValidateLimitsAcceptsValid(t *testing.T) {
	inv := validInventory()
	inv.Capabilities = map[Capability]CapabilityStatus{CapAPIUsage: {Available: true, Partial: true,
		Reason: strings.Repeat("r", MaxReasonBytes), Skipped: []string{"policy/v1beta1 PodSecurityPolicy"}}}
	inv.HelmReleases[0].KubeVersion = strings.Repeat("<", MaxStringBytes)
	inv.APIUsage[0].Objects[1].Manager = strings.Repeat("m", MaxManagerBytes)
	for i := range MaxObjectRefs - len(inv.APIUsage[0].Objects) {
		inv.APIUsage[0].Objects = append(inv.APIUsage[0].Objects, ObjectRef{Name: fmt.Sprint("o", i)})
	}
	inv.APIUsage = append(inv.APIUsage, APIUsage{Group: "policy", Version: "v1", Kind: "PodSecurityPolicy"})
	for i := range MaxUnrecognizedImages {
		inv.UnrecognizedImages = append(inv.UnrecognizedImages, fmt.Sprint("registry.example.com/i", i))
	}
	if err := inv.ValidateLimits(); err != nil {
		t.Fatalf("ValidateLimits() = %v, want nil", err)
	}
	if err := (Inventory{}).ValidateLimits(); err != nil {
		t.Fatalf("empty inventory: %v", err)
	}
}

func TestValidateLimitsRefusesBeyond(t *testing.T) {
	long := strings.Repeat("x", MaxStringBytes+1)
	for _, tc := range []struct {
		name  string
		edit  func(*Inventory)
		field string
	}{
		{"objects over MaxObjectRefs", func(inv *Inventory) {
			for range MaxObjectRefs {
				inv.APIUsage[0].Objects = append(inv.APIUsage[0].Objects, ObjectRef{})
			}
		}, "apiUsage[0].objects"},
		{"a group/version/kind twice", func(inv *Inventory) {
			inv.APIUsage = append(inv.APIUsage, APIUsage{Group: "policy", Version: "v1beta1", Kind: "PodSecurityPolicy"})
		}, "apiUsage[1]"},
		{"a manifest group/version/kind twice", func(inv *Inventory) {
			inv.HelmReleases[0].ManifestAPIs = append(inv.HelmReleases[0].ManifestAPIs, inv.HelmReleases[0].ManifestAPIs[0])
		}, "helmReleases[0].manifestApis[1]"},
		{"an authorship-unknown group/version/kind twice", func(inv *Inventory) {
			inv.APIAuthorshipUnknown = append(inv.APIAuthorshipUnknown, inv.APIAuthorshipUnknown[0])
		}, "apiAuthorshipUnknown[1]"},
		{"a CRD version twice", func(inv *Inventory) { inv.CRDs[0].Usage = append(inv.CRDs[0].Usage, inv.CRDs[0].Usage[0]) }, "crds[0].usage[1]"},
		{"a manager over 128 bytes", func(inv *Inventory) { inv.APIUsage[0].Objects[1].Manager = strings.Repeat("'", 129) }, "apiUsage[0].objects[1].manager"},
		{"a manager with a control character", func(inv *Inventory) { inv.CRDs[0].Usage[0].Objects[0].Manager = "m\x01" }, "crds[0].usage[0].objects[0].manager"},
		{"unrecognized images over the cap", func(inv *Inventory) {
			inv.UnrecognizedImages = make([]string, MaxUnrecognizedImages+1)
		}, "unrecognizedImages"},
		{"a kubeVersion over the string limit", func(inv *Inventory) { inv.HelmReleases[0].KubeVersion = long }, "helmReleases[0].kubeVersion"},
		{"a capability reason over its limit", func(inv *Inventory) {
			inv.Capabilities = map[Capability]CapabilityStatus{CapHelm: {Reason: strings.Repeat("r", MaxReasonBytes+1)}}
		}, `capabilities["helm"].reason`},
		{"a capability name over the string limit", func(inv *Inventory) {
			inv.Capabilities = map[Capability]CapabilityStatus{Capability(long): {}}
		}, "capabilities (a key)"},
		{"a skipped API over the string limit", func(inv *Inventory) {
			inv.Capabilities = map[Capability]CapabilityStatus{CapAPIUsage: {Skipped: []string{"a", long}}}
		}, `capabilities["api-usage"].skipped[1]`},
		{"a CRD deprecation warning", func(inv *Inventory) { inv.CRDs[0].Versions = []CRDVersion{{Name: "v1", DeprecationWarning: long}} }, "crds[0].versions[0].deprecationWarning"},
		{"a node runtime", func(inv *Inventory) { inv.Nodes[0].ContainerRuntime = long }, "nodes[0].containerRuntime"},
		{"capabilities over the cap", func(inv *Inventory) {
			inv.Capabilities = map[Capability]CapabilityStatus{}
			for i := range MaxCapabilities + 1 {
				inv.Capabilities[Capability(fmt.Sprint("c", i))] = CapabilityStatus{}
			}
		}, "capabilities"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inv := validInventory()
			tc.edit(&inv)
			err := inv.ValidateLimits()
			var le *LimitError
			if !errors.As(err, &le) || le.Field != tc.field {
				t.Fatalf("ValidateLimits() = %.300v, want a LimitError at %s", err, tc.field)
			}
			if len(err.Error()) > 1024 {
				t.Fatalf("message is %d bytes, want a short one", len(err.Error()))
			}
		})
	}
}

// A collector copies some text whole from what it reads, and Kubernetes
// lets it be longer than the limits: a capability's reason joins one error
// per resource it could not read, and the ignore annotations may hold up
// to 256 KiB. CutFreeText cuts those to the limits, so ValidateLimits
// never refuses a genuine inventory for them.
func TestCutFreeTextCutsToTheLimits(t *testing.T) {
	inv := validInventory()
	var failures []string
	for i := range 400 { // a CRD collector's forbidden lists, ~200 bytes each
		failures = append(failures, fmt.Sprintf(`list example%03d.io/v1beta1 widgets: widgets.example%03d.io is forbidden: User "system:serviceaccount:upgradescope:upgradescope-agent" cannot list resource "widgets"`, i, i))
	}
	inv.Capabilities = map[Capability]CapabilityStatus{CapCRDs: {Available: true, Partial: true,
		Reason: strings.Join(failures, "; "), Skipped: []string{strings.Repeat("é", MaxStringBytes)}}}
	inv.APIUsage[0].Objects[0].Ignore = "deprecated-api, " + strings.Repeat("k", 300<<10)
	inv.APIUsage[0].Objects[0].IgnoreReason = strings.Repeat("€", 100<<10)
	inv.CRDs[0].Usage[0].Objects[0].IgnoreReason = strings.Repeat("r", 256<<10)
	inv.HelmReleases[0].ManifestAPIs[0].Objects[0].Ignore = strings.Repeat("a,", 20<<10)
	inv.GitOpsCharts[0].Version = strings.Repeat("v", 300<<10) // free text of a custom resource
	inv.GitOpsCharts[0].Repo = strings.Repeat("é", MaxStringBytes)
	if err := inv.ValidateLimits(); err == nil {
		t.Fatal("the uncut inventory is within the limits; the test proves nothing")
	}
	if !inv.CutFreeText() {
		t.Fatal("CutFreeText() = false, want true")
	}
	if err := inv.ValidateLimits(); err != nil {
		t.Fatalf("after CutFreeText, ValidateLimits() = %v", err)
	}
	r := inv.Capabilities[CapCRDs].Reason
	if !strings.HasPrefix(r, failures[0]) || !strings.HasSuffix(r, cutMark) || !utf8.ValidString(inv.Capabilities[CapCRDs].Skipped[0]) {
		t.Errorf("reason cut to %d bytes ending %q; want the first failures, then %q, and valid UTF-8", len(r), r[len(r)-40:], cutMark)
	}
	if o := inv.APIUsage[0].Objects[0]; o.Ignore != "deprecated-api" || !utf8.ValidString(o.IgnoreReason) {
		t.Errorf("ignore = %.40q, want only its whole tokens; ignore-reason valid UTF-8: %v", o.Ignore, utf8.ValidString(o.IgnoreReason))
	}
	if ig := inv.HelmReleases[0].ManifestAPIs[0].Objects[0].Ignore; len(ig) > MaxStringBytes || !strings.HasSuffix(ig, ",a") {
		t.Errorf("ignore of many tokens cut to %d bytes ending %q, want whole tokens", len(ig), ig[len(ig)-4:])
	}
	if c := inv.GitOpsCharts[0]; len(c.Version) > MaxStringBytes || !strings.HasSuffix(c.Version, cutMark) || len(c.Repo) > MaxStringBytes || !utf8.ValidString(c.Repo) {
		t.Errorf("gitops chart version cut to %d bytes, repo to %d (valid UTF-8: %v), want both within %d, cut", len(c.Version), len(c.Repo), utf8.ValidString(c.Repo), MaxStringBytes)
	}
	if inv.CutFreeText() {
		t.Error("a second CutFreeText() = true, want nothing left to cut")
	}
	within := validInventory()
	if within.CutFreeText() {
		t.Error("CutFreeText() of an inventory within the limits = true")
	}
}
