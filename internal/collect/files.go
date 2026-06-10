package collect

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// manifestDoc decodes only TypeMeta + the ObjectMeta fields we need.
type manifestDoc struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
}

// CollectFiles builds an Inventory from rendered manifests on disk
// (--files mode, CI gating). Walks *.yaml/*.yml/*.json recursively in
// lexical order. Only api-usage is assessable offline; every other
// capability degrades with reason "files mode". Malformed YAML is a hard
// error: in CI, silently skipping a bad manifest would be a false pass.
func CollectFiles(dir string) (inventory.Inventory, error) {
	inv := inventory.Inventory{
		SchemaVersion: 1,
		ClusterID:     "files",
		CollectedAt:   time.Now().UTC(),
		Capabilities: map[inventory.Capability]inventory.CapabilityStatus{
			inventory.CapAPIUsage:        {Available: true},
			inventory.CapDeprecatedCalls: {Available: false, Reason: "files mode"},
			inventory.CapHelm:            {Available: false, Reason: "files mode"},
			inventory.CapAddOns:          {Available: false, Reason: "files mode"},
			inventory.CapVersions:        {Available: false, Reason: "files mode"},
		},
	}

	type gvk struct{ group, version, kind string }
	counts := map[gvk]*inventory.APIUsage{}

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".yaml" && ext != ".yml" && ext != ".json" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		// Document splitting via apimachinery's YAMLReader — the same
		// kubectl-compatible splitter (handles "--- # comment" separator
		// lines that a plain `^---$` split would corrupt). JSON files are
		// a single document and pass through unchanged.
		// Known limitation: kind:List items are not expanded; the List
		// object itself is counted.
		reader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
		for {
			raw, rerr := reader.Read()
			if errors.Is(rerr, io.EOF) {
				break
			}
			if rerr != nil {
				return fmt.Errorf("%s: %w", path, rerr)
			}
			if strings.TrimSpace(string(raw)) == "" {
				continue
			}
			var doc manifestDoc
			if err := yaml.Unmarshal(raw, &doc); err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			if doc.APIVersion == "" || doc.Kind == "" {
				continue // comment-only or non-Kubernetes document
			}
			group, version := "", doc.APIVersion
			if g, v, ok := strings.Cut(doc.APIVersion, "/"); ok {
				group, version = g, v
			}
			k := gvk{group, version, doc.Kind}
			u := counts[k]
			if u == nil {
				u = &inventory.APIUsage{Group: group, Version: version, Kind: doc.Kind, Namespaces: map[string]int{}}
				counts[k] = u
			}
			u.Count++
			u.Namespaces[doc.Metadata.Namespace]++ // cluster-scoped/unset → key ""
		}
		return nil
	})
	if err != nil {
		return inv, err
	}

	for _, u := range counts {
		inv.APIUsage = append(inv.APIUsage, *u)
	}
	sort.Slice(inv.APIUsage, func(i, j int) bool {
		a, b := inv.APIUsage[i], inv.APIUsage[j]
		if a.Group != b.Group {
			return a.Group < b.Group
		}
		if a.Version != b.Version {
			return a.Version < b.Version
		}
		return a.Kind < b.Kind
	})
	return inv, nil
}
