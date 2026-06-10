// registry/types.go
package registry

// AddOn is one entry in the add-on EOL/compatibility registry (schema_version 1).
type AddOn struct {
	SchemaVersion  int      `json:"schema_version" yaml:"schema_version"` // 1
	ID             string   `json:"id" yaml:"id"`
	DisplayName    string   `json:"display_name" yaml:"display_name"`
	Matchers       Matchers `json:"matchers" yaml:"matchers"`
	Support        Support  `json:"support" yaml:"support"`
	Compat         []Compat `json:"compat,omitempty" yaml:"compat,omitempty"`
	Recommendation string   `json:"recommendation,omitempty" yaml:"recommendation,omitempty"`
}

type Matchers struct {
	Images []string `json:"images,omitempty" yaml:"images,omitempty"` // repo prefix match, tag = version
	Charts []string `json:"charts,omitempty" yaml:"charts,omitempty"` // exact chart name
}

type Support struct {
	Status    string   `json:"status" yaml:"status"`                         // "supported" | "eol" | "unknown"
	EOLDate   string   `json:"eol_date,omitempty" yaml:"eol_date,omitempty"` // RFC3339 date "2026-03-24"
	Citations []string `json:"citations" yaml:"citations"`                   // ≥1 required when status != unknown
}

// Compat maps a chart/app version range to a supported K8s range.
type Compat struct {
	Range     string   `json:"range" yaml:"range"`     // semver constraint, e.g. ">=4.0.0"
	K8sMin    string   `json:"k8s_min" yaml:"k8s_min"` // "1.21"
	K8sMax    string   `json:"k8s_max" yaml:"k8s_max"` // "1.36" (inclusive)
	Citations []string `json:"citations" yaml:"citations"`
}
