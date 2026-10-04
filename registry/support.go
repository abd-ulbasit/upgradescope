// registry/support.go
package registry

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"sort"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

// ProviderSchemaVersion is the only managed-provider schema version this
// build understands.
const ProviderSchemaVersion = 1

// ProviderIDs are the managed Kubernetes providers the dataset covers, as
// inventory.Provider names them.
var ProviderIDs = []string{"eks", "gke", "aks"}

// ProviderSupport is one managed Kubernetes provider's support calendar:
// when each Kubernetes minor leaves standard support and when extended
// support ends, and what extended support costs where the provider
// publishes a price. It is a dataset of its own beside the add-on entries:
// those describe software a cluster runs, this describes the service the
// control plane is bought from.
type ProviderSupport struct {
	SchemaVersion int    `json:"schema_version" yaml:"schema_version"` // 1
	ID            string `json:"id" yaml:"id"`                         // one of ProviderIDs, = file name
	DisplayName   string `json:"display_name" yaml:"display_name"`
	// EndoflifeProduct, when set, is the endoflife.date slug the Versions
	// are generated from by tools/eol-sync. Hand-curated entries (GKE,
	// whose extended-support end endoflife.date does not publish) leave it
	// empty.
	EndoflifeProduct string `json:"endoflife_product,omitempty" yaml:"endoflife_product,omitempty"`
	// ExtendedSupportNote says in a sentence what extended support means
	// at this provider, for the finding that names it (GKE bills it only
	// for clusters on its Extended release channel; AKS has no automatic
	// extended support, only an opt-in Long Term Support plan).
	ExtendedSupportNote string `json:"extended_support_note,omitempty" yaml:"extended_support_note,omitempty"`
	// ExtendedSupportCondition is set where extended support is not
	// automatic: the configuration a cluster must have for the extended
	// window to apply to it, as a clause that completes "only if ..." (GKE:
	// "the cluster is on the Extended release channel"; AKS: "Long Term
	// Support is enabled"). The collector cannot see it, so findings for
	// the provider state the window conditionally instead of asserting
	// that this cluster is in it. Empty for EKS, where extended support is
	// the default.
	ExtendedSupportCondition string `json:"extended_support_condition,omitempty" yaml:"extended_support_condition,omitempty"`
	// Citations are the pages the Versions dates come from.
	Citations []string `json:"citations" yaml:"citations"`
	// Versions are the provider's Kubernetes minors, newest first. When
	// endoflife_product is set tools/eol-sync owns this block.
	Versions []SupportWindow `json:"versions" yaml:"versions"`
	// Pricing is hand-entered from the provider's own pricing page; a
	// provider without a price stated unambiguously leaves it out, and no
	// cost is then reported for it.
	Pricing *Pricing `json:"pricing,omitempty" yaml:"pricing,omitempty"`
}

// SupportWindow is one Kubernetes minor's support calendar at a provider.
// Both dates are the day the phase ends: the minor is in standard support
// before StandardEnd, in extended support from StandardEnd until ExtendedEnd
// (the day the provider upgrades the cluster or stops supporting it), and
// out of support from ExtendedEnd. An empty ExtendedEnd means the provider
// offered no extended support for the minor.
type SupportWindow struct {
	Minor       string `json:"minor" yaml:"minor"` // "1.34"
	StandardEnd string `json:"standard_end" yaml:"standard_end"`
	ExtendedEnd string `json:"extended_end,omitempty" yaml:"extended_end,omitempty"`
}

// UnmarshalJSON decodes a window strictly, and refuses an unquoted minor
// (YAML reads 1.30 as the number 1.3, another minor).
func (w *SupportWindow) UnmarshalJSON(b []byte) error {
	type plain SupportWindow
	var p plain
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) && typeErr.Value == "number" {
			return fmt.Errorf("version field %q: quote versions (\"1.30\", not 1.30)", typeErr.Field)
		}
		return err
	}
	*w = SupportWindow(p)
	return nil
}

// Pricing is a provider's published control-plane price in USD per cluster
// hour. It is a list price: it varies by contract, and a provider may
// change it, so every use states AsOf.
type Pricing struct {
	Currency               string  `json:"currency" yaml:"currency"` // "USD"
	StandardPerClusterHour float64 `json:"standard_per_cluster_hour" yaml:"standard_per_cluster_hour"`
	// ExtendedPerClusterHour is the whole price while in extended support,
	// the standard fee included (EKS: 0.60 = 0.10 + 0.50).
	ExtendedPerClusterHour float64 `json:"extended_per_cluster_hour" yaml:"extended_per_cluster_hour"`
	AsOf                   string  `json:"as_of" yaml:"as_of"` // YYYY-MM-DD the page was read
	// Note is a caveat on whom the price applies to, shown with the cost.
	Note      string   `json:"note,omitempty" yaml:"note,omitempty"`
	Citations []string `json:"citations" yaml:"citations"`
}

//go:embed data/providers/*.yaml
var providerFS embed.FS

// LoadProviders parses and validates every embedded provider entry,
// sorted by ID. Any parse or validation error fails the whole load.
func LoadProviders() ([]ProviderSupport, error) {
	return loadProvidersFS(providerFS, "data/providers")
}

func loadProvidersFS(fsys fs.FS, dir string) ([]ProviderSupport, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("registry: read dir %s: %w", dir, err)
	}
	var (
		out  []ProviderSupport
		errs []error
	)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), ".yml") {
			errs = append(errs, fmt.Errorf("registry: %s/%s: registry entries must use the .yaml extension (rename to .yaml)", dir, e.Name()))
			continue
		}
		if !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		name := dir + "/" + e.Name()
		raw, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("registry: read %s: %w", name, err)
		}
		var p ProviderSupport
		if err := yaml.UnmarshalStrict(raw, &p); err != nil {
			errs = append(errs, fmt.Errorf("registry: parse %s: %w", name, err))
			continue
		}
		if stem := strings.TrimSuffix(e.Name(), ".yaml"); p.ID != stem {
			errs = append(errs, fmt.Errorf("registry: %s: id %q must equal the file name (%s)", name, p.ID, stem))
		}
		for _, verr := range ValidateProvider(p) {
			errs = append(errs, fmt.Errorf("registry: %s: %w", name, verr))
		}
		out = append(out, p)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// ValidateProvider checks one ProviderSupport against the schema_version 1
// rules and returns every violation (empty means valid).
func ValidateProvider(p ProviderSupport) []error {
	var errs []error
	if p.SchemaVersion != ProviderSchemaVersion {
		errs = append(errs, fmt.Errorf("%s: schema_version must be %d, got %d", p.ID, ProviderSchemaVersion, p.SchemaVersion))
	}
	if !slices.Contains(ProviderIDs, p.ID) {
		errs = append(errs, fmt.Errorf("id must be one of %s, got %q", strings.Join(ProviderIDs, ", "), p.ID))
	}
	if p.EndoflifeProduct != "" && !eolSlugPattern.MatchString(p.EndoflifeProduct) {
		errs = append(errs, fmt.Errorf("%s: endoflife_product %q must be a lowercase endoflife.date slug", p.ID, p.EndoflifeProduct))
	}
	if c := p.ExtendedSupportCondition; c != "" {
		low := strings.ToLower(c)
		if c != strings.TrimSpace(c) || strings.HasSuffix(c, ".") || strings.HasPrefix(low, "if ") || strings.HasPrefix(low, "only if ") {
			errs = append(errs, fmt.Errorf("%s: extended_support_condition %q must be a bare clause with no leading \"if\" and no trailing period (it completes \"only if ...\")", p.ID, c))
		}
	}
	errs = append(errs, validateCitations(p.ID, p.Citations)...)
	if len(p.Versions) == 0 {
		errs = append(errs, fmt.Errorf("%s: at least one version required", p.ID))
	}
	seen := map[string]bool{}
	for i, w := range p.Versions {
		where := fmt.Sprintf("%s: versions[%d] (%s)", p.ID, i, w.Minor)
		if !k8sVerPattern.MatchString(w.Minor) {
			errs = append(errs, fmt.Errorf("%s: minor must be MAJOR.MINOR such as \"1.34\"", where))
		} else if seen[w.Minor] {
			errs = append(errs, fmt.Errorf("%s: duplicate minor %q", where, w.Minor))
		}
		seen[w.Minor] = true
		std, stdErr := time.Parse("2006-01-02", w.StandardEnd)
		if stdErr != nil {
			errs = append(errs, fmt.Errorf("%s: standard_end %q must be a valid YYYY-MM-DD date", where, w.StandardEnd))
		}
		if w.ExtendedEnd == "" {
			continue
		}
		ext, extErr := time.Parse("2006-01-02", w.ExtendedEnd)
		switch {
		case extErr != nil:
			errs = append(errs, fmt.Errorf("%s: extended_end %q must be a valid YYYY-MM-DD date", where, w.ExtendedEnd))
		case stdErr == nil && !ext.After(std):
			errs = append(errs, fmt.Errorf("%s: extended_end %s must be after standard_end %s", where, w.ExtendedEnd, w.StandardEnd))
		}
	}
	if p.Pricing != nil {
		errs = append(errs, validatePricing(p.ID, *p.Pricing)...)
	}
	return errs
}

func validatePricing(id string, pr Pricing) []error {
	var errs []error
	where := id + ": pricing"
	if pr.Currency != "USD" {
		errs = append(errs, fmt.Errorf("%s: currency must be \"USD\", got %q", where, pr.Currency))
	}
	if pr.StandardPerClusterHour <= 0 {
		errs = append(errs, fmt.Errorf("%s: standard_per_cluster_hour must be positive", where))
	}
	if pr.ExtendedPerClusterHour <= pr.StandardPerClusterHour {
		errs = append(errs, fmt.Errorf("%s: extended_per_cluster_hour must exceed standard_per_cluster_hour (it includes it)", where))
	}
	if _, err := time.Parse("2006-01-02", pr.AsOf); err != nil {
		errs = append(errs, fmt.Errorf("%s: as_of %q must be the YYYY-MM-DD date the price was read", where, pr.AsOf))
	}
	errs = append(errs, validateCitations(where, pr.Citations)...)
	return errs
}
