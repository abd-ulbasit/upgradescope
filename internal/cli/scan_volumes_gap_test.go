package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	k8sversion "k8s.io/apimachinery/pkg/version"
	discoveryfake "k8s.io/client-go/discovery/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	metadatafake "k8s.io/client-go/metadata/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/abd-ulbasit/upgradescope/internal/collect"
	"github.com/abd-ulbasit/upgradescope/internal/engine"
	"github.com/abd-ulbasit/upgradescope/internal/kb"
)

// #374 end to end, collect through the gate: a 1.30 cluster with an rbd
// PersistentVolume, scanned at 1.31. With the PersistentVolume and
// StorageClass lists allowed it is blocked by volume-plugin/rbd, exit 2.
// With both refused the rbd PV goes unseen, and the scan must not read
// READY yes: the verdict is unknown, exit 2, unless --allow-incomplete.
func TestScanRefusedVolumeListsHidingRBDIsNotReady(t *testing.T) {
	k, err := kb.Load()
	if err != nil {
		t.Fatal(err)
	}
	const server = "v1.30.4"
	stub := func(refuse bool) func(scanOptions) (engine.Report, error) {
		return func(opts scanOptions) (engine.Report, error) {
			cs := kubefake.NewClientset(
				&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID("uid-rbd")}},
				&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n"}, Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{KubeletVersion: server}}},
				&corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "pv-rbd"}, Spec: corev1.PersistentVolumeSpec{
					PersistentVolumeSource: corev1.PersistentVolumeSource{RBD: &corev1.RBDPersistentVolumeSource{CephMonitors: []string{"10.0.0.1:6789"}, RBDImage: "x"}}}},
			)
			if refuse {
				for _, r := range []schema.GroupResource{{Resource: "persistentvolumes"}, {Group: "storage.k8s.io", Resource: "storageclasses"}} {
					cs.PrependReactor("list", r.Resource, func(k8stesting.Action) (bool, runtime.Object, error) {
						return true, nil, apierrors.NewForbidden(r, "", errors.New("RBAC"))
					})
				}
			}
			disc := cs.Discovery().(*discoveryfake.FakeDiscovery)
			disc.FakedServerVersion = &k8sversion.Info{GitVersion: server}
			disc.Resources = []*metav1.APIResourceList{{GroupVersion: "v1",
				APIResources: []metav1.APIResource{{Name: "pods", Kind: "Pod", Namespaced: true, Verbs: metav1.Verbs{"list"}}}}}
			scheme := runtime.NewScheme()
			utilruntime.Must(metav1.AddMetaToScheme(scheme))
			inv := collect.Collect(context.Background(), collect.Clients{Kube: cs, Discovery: disc,
				Metadata: metadatafake.NewSimpleMetadataClient(scheme)}, k, collect.Options{})
			return evaluateScan(inv, k, opts, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)), nil
		}
	}
	for _, tc := range []struct {
		name   string
		refuse bool
		args   []string
		code   int
		err    error
		table  string
	}{
		{"lists allowed: blocked", false, nil, 2, ErrGateFailed, "READY  no\n"},
		{"lists refused: unknown", true, nil, 2, ErrIncomplete, "READY  unknown (required checks were not assessed)\n  volumes (partial, required)"},
		{"lists refused, --allow-incomplete", true, []string{"--allow-incomplete"}, 0, nil, "READY  unknown (required checks were not assessed)\n  volumes (partial, required)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := execScan(t, append([]string{"--target", "1.31"}, tc.args...), stub(tc.refuse))
			if got := ExitCode(err); got != tc.code || (tc.err != nil && !errors.Is(err, tc.err)) {
				t.Fatalf("err = %v (exit %d), want %v, exit %d\n%s", err, got, tc.err, tc.code, out)
			}
			if !strings.Contains(out, tc.table) {
				t.Errorf("table lacks %q:\n%s", tc.table, out)
			}
		})
	}
}
