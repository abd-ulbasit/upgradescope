package collect

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"fmt"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"github.com/abd-ulbasit/upgradescope/internal/inventory"
)

// helmSecret builds a Secret exactly as Helm v3 stores releases: the
// "release" key holds base64(gzip(JSON)) — client-go strips the outer
// Secret base64, leaving this inner base64 string.
func helmSecret(t *testing.T, ns, release string, rev int, chartName, chartVersion, appVersion, status string) *corev1.Secret {
	t.Helper()
	payload := fmt.Sprintf(
		`{"name":%q,"info":{"status":%q},"chart":{"metadata":{"name":%q,"version":%q,"appVersion":%q}}}`,
		release, status, chartName, chartVersion, appVersion)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("sh.helm.release.v1.%s.v%d", release, rev),
			Namespace: ns,
		},
		Type: "helm.sh/release.v1",
		Data: map[string][]byte{"release": []byte(base64.StdEncoding.EncodeToString(gz.Bytes()))},
	}
}

func TestCollectHelmLatestRevisionPerRelease(t *testing.T) {
	cs := kubefake.NewClientset(
		helmSecret(t, "ingress-nginx", "ingress-nginx", 1, "ingress-nginx", "4.7.0", "1.8.1", "superseded"),
		helmSecret(t, "ingress-nginx", "ingress-nginx", 2, "ingress-nginx", "4.7.1", "1.8.4", "deployed"),
		helmSecret(t, "cert-manager", "cert-manager", 1, "cert-manager", "v1.13.0", "v1.13.0", "deployed"),
		&corev1.Secret{ // not a helm secret: must be ignored
			ObjectMeta: metav1.ObjectMeta{Name: "db-creds", Namespace: "shop"},
			Type:       corev1.SecretTypeOpaque,
			Data:       map[string][]byte{"password": []byte("hunter2")},
		},
	)

	var inv inventory.Inventory
	if err := collectHelm(context.Background(), cs, &inv); err != nil {
		t.Fatal(err)
	}
	want := []inventory.HelmRelease{
		{Name: "cert-manager", Namespace: "cert-manager", ChartName: "cert-manager", ChartVersion: "v1.13.0", AppVersion: "v1.13.0", Status: "deployed"},
		{Name: "ingress-nginx", Namespace: "ingress-nginx", ChartName: "ingress-nginx", ChartVersion: "4.7.1", AppVersion: "1.8.4", Status: "deployed"},
	}
	if !reflect.DeepEqual(inv.HelmReleases, want) {
		t.Errorf("releases = %#v\nwant      %#v", inv.HelmReleases, want)
	}
}

func TestCollectHelmSkipsCorruptSecretKeepsValid(t *testing.T) {
	cs := kubefake.NewClientset(
		helmSecret(t, "cert-manager", "cert-manager", 1, "cert-manager", "v1.13.0", "v1.13.0", "deployed"),
		&corev1.Secret{ // helm-typed but corrupt payload: skipped, not fatal
			ObjectMeta: metav1.ObjectMeta{Name: "sh.helm.release.v1.broken.v1", Namespace: "shop"},
			Type:       "helm.sh/release.v1",
			Data:       map[string][]byte{"release": []byte("%%% not base64 %%%")},
		},
		helmSecret(t, "ingress-nginx", "ingress-nginx", 1, "ingress-nginx", "4.7.1", "1.8.4", "deployed"),
	)

	var inv inventory.Inventory
	if err := collectHelm(context.Background(), cs, &inv); err != nil {
		t.Fatalf("one corrupt secret must not fail the capability: %v", err)
	}
	want := []inventory.HelmRelease{
		{Name: "cert-manager", Namespace: "cert-manager", ChartName: "cert-manager", ChartVersion: "v1.13.0", AppVersion: "v1.13.0", Status: "deployed"},
		{Name: "ingress-nginx", Namespace: "ingress-nginx", ChartName: "ingress-nginx", ChartVersion: "4.7.1", AppVersion: "1.8.4", Status: "deployed"},
	}
	if !reflect.DeepEqual(inv.HelmReleases, want) {
		t.Errorf("releases = %#v\nwant      %#v (valid secrets must survive the corrupt one)", inv.HelmReleases, want)
	}
}

func TestDecodeHelmReleaseRejectsNonGzip(t *testing.T) {
	if _, err := decodeHelmRelease([]byte(base64.StdEncoding.EncodeToString([]byte("plain")))); err == nil {
		t.Fatal("want error for payload without gzip magic bytes")
	}
}
