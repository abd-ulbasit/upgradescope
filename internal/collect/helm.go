package collect

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

const helmSecretType = "helm.sh/release.v1"

// helmReleaseDoc is the minimal slice of Helm's release JSON we decode.
// No Helm SDK dependency.
type helmReleaseDoc struct {
	Name string `json:"name"`
	Info struct {
		Status string `json:"status"`
	} `json:"info"`
	Chart struct {
		Metadata struct {
			Name       string `json:"name"`
			Version    string `json:"version"`
			AppVersion string `json:"appVersion"`
		} `json:"metadata"`
	} `json:"chart"`
}

// collectHelm reads Helm v3 release Secrets and keeps only the latest
// revision per (namespace, release name) — max N from the secret name
// suffix "sh.helm.release.v1.<name>.v<N>".
func collectHelm(ctx context.Context, kube kubernetes.Interface, inv *inventory.Inventory) error {
	type candidate struct {
		rev int
		rel inventory.HelmRelease
	}
	latest := map[string]candidate{} // "<namespace>/<release name>"

	opts := metav1.ListOptions{
		FieldSelector: "type=" + helmSecretType,
		Limit:         listPageSize,
	}
	for {
		secrets, err := kube.CoreV1().Secrets(metav1.NamespaceAll).List(ctx, opts)
		if err != nil {
			return fmt.Errorf("list helm release secrets: %w", err)
		}
		for i := range secrets.Items {
			s := &secrets.Items[i]
			if s.Type != corev1.SecretType(helmSecretType) { // fake clients ignore field selectors
				continue
			}
			doc, err := decodeHelmRelease(s.Data["release"])
			if err != nil {
				continue // one corrupt secret must not fail the capability
			}
			rev := releaseRevision(s.Name)
			key := s.Namespace + "/" + doc.Name
			if cur, ok := latest[key]; ok && cur.rev >= rev {
				continue
			}
			latest[key] = candidate{rev: rev, rel: inventory.HelmRelease{
				Name:         doc.Name,
				Namespace:    s.Namespace,
				ChartName:    doc.Chart.Metadata.Name,
				ChartVersion: doc.Chart.Metadata.Version,
				AppVersion:   doc.Chart.Metadata.AppVersion,
				Status:       doc.Info.Status,
			}}
		}
		if secrets.Continue == "" {
			break
		}
		opts.Continue = secrets.Continue
	}

	rels := make([]inventory.HelmRelease, 0, len(latest))
	for _, c := range latest {
		rels = append(rels, c.rel)
	}
	sort.Slice(rels, func(i, j int) bool {
		if rels[i].Namespace != rels[j].Namespace {
			return rels[i].Namespace < rels[j].Namespace
		}
		return rels[i].Name < rels[j].Name
	})
	if len(rels) > 0 {
		inv.HelmReleases = rels
	}
	return nil
}

// releaseRevision parses N from "sh.helm.release.v1.<name>.v<N>"; 0 if absent.
func releaseRevision(secretName string) int {
	i := strings.LastIndex(secretName, ".v")
	if i < 0 {
		return 0
	}
	n, err := strconv.Atoi(secretName[i+2:])
	if err != nil {
		return 0
	}
	return n
}

// decodeHelmRelease decodes Secret.Data["release"]. client-go has already
// base64-decoded the Secret data once; what remains is a base64 string
// wrapping gzip(JSON). Order: base64 → gzip magic check → gunzip → JSON.
func decodeHelmRelease(data []byte) (helmReleaseDoc, error) {
	var doc helmReleaseDoc
	raw, err := base64.StdEncoding.DecodeString(string(data))
	if err != nil {
		return doc, fmt.Errorf("base64: %w", err)
	}
	if len(raw) < 3 || raw[0] != 0x1f || raw[1] != 0x8b || raw[2] != 0x08 {
		return doc, fmt.Errorf("release payload is not gzip")
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return doc, fmt.Errorf("gunzip: %w", err)
	}
	defer zr.Close()
	jsonBytes, err := io.ReadAll(zr)
	if err != nil {
		return doc, fmt.Errorf("gunzip read: %w", err)
	}
	if err := json.Unmarshal(jsonBytes, &doc); err != nil {
		return doc, fmt.Errorf("release json: %w", err)
	}
	return doc, nil
}
