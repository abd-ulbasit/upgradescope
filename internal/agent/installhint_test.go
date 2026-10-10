package agent

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/abd-ulbasit/upgradescope/internal/crd"
)

func withAgentVersion(t *testing.T, v string) {
	t.Helper()
	old := AgentVersion
	AgentVersion = v
	t.Cleanup(func() { AgentVersion = old })
}

// forbidCRDCreate is an apiextensions client under the chart's RBAC: no CRD
// create. withLegacy installs the pre-v0.2.0 CRD first.
func forbidCRDCreate(withLegacy bool) *apiextfake.Clientset {
	var apiext *apiextfake.Clientset
	if withLegacy {
		apiext = apiextfake.NewClientset(&apiextensionsv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{Name: crd.LegacyCRDName}})
	} else {
		apiext = apiextfake.NewClientset()
	}
	apiext.PrependReactor("create", "customresourcedefinitions",
		func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(
				schema.GroupResource{Group: "apiextensions.k8s.io", Resource: "customresourcedefinitions"}, "x", errors.New("RBAC"))
		})
	return apiext
}

// The fix for a missing CRD is in the error itself: with the legacy CRD
// installed or not, the startup error carries the exact command, at the
// binary's release, since Helm does not install crds/ on upgrade.
func TestRunNamesTheInstallCommandWhenTheCRDIsMissing(t *testing.T) {
	withAgentVersion(t, "0.2.0")
	for name, withLegacy := range map[string]bool{"no CRD at all": false, "only the legacy CRD": true} {
		err := Run(context.Background(), fakeClients(t, "v1.35.2"), fakeDyn(), forbidCRDCreate(withLegacy), mustKB(t), Config{})
		if !errors.Is(err, crd.ErrCRDNotInstalled) {
			t.Fatalf("%s: Run err = %v, want crd.ErrCRDNotInstalled", name, err)
		}
		want := "kubectl apply -f https://raw.githubusercontent.com/abd-ulbasit/upgradescope/v0.2.0/deploy/chart/crds/" + crd.ManifestFile
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%s: Run err = %v\nwant the command %q", name, err, want)
		}
	}
}

// A dev build has no release to fetch from: the error says which tag to use.
func TestRunDevBuildSaysWhichTagToInstallTheCRDFrom(t *testing.T) {
	withAgentVersion(t, "dev")
	err := Run(context.Background(), fakeClients(t, "v1.35.2"), fakeDyn(), forbidCRDCreate(false), mustKB(t), Config{})
	if !errors.Is(err, crd.ErrCRDNotInstalled) {
		t.Fatalf("Run err = %v, want crd.ErrCRDNotInstalled", err)
	}
	for _, want := range []string{"/<tag>/deploy/chart/crds/", "dev build", "replace <tag>"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Run err = %v\nwant %q", err, want)
		}
	}
}

// With --manage-crd=false the agent stays up and every tick fails; the
// tick error, which /readyz and the log carry, names the same command.
func TestTickNamesTheInstallCommandWhenTheCRDIsMissing(t *testing.T) {
	withAgentVersion(t, "0.2.0")
	dyn := fakeDyn().(*dynamicfake.FakeDynamicClient)
	dyn.PrependReactor("create", crd.Plural, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Group: crd.Group, Resource: crd.Plural}, "")
	})
	r := testRunner(t, dyn, "")
	rep := r.runTick(context.Background())
	if !errors.Is(rep.err, crd.ErrCRDNotInstalled) {
		t.Fatalf("tick err = %v, want crd.ErrCRDNotInstalled", rep.err)
	}
	want := "kubectl apply -f https://raw.githubusercontent.com/abd-ulbasit/upgradescope/v0.2.0/deploy/chart/crds/" + crd.ManifestFile
	if !strings.Contains(rep.err.Error(), want) {
		t.Errorf("tick err = %v\nwant the command %q", rep.err, want)
	}

	// The observer records that error and /readyz serves it, as the claim says.
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	o := newTestObserver(t, &now)
	o.record(rep)
	code, body := serve(t, o.handler(), "/readyz")
	if code != http.StatusServiceUnavailable || !strings.Contains(body, want) {
		t.Errorf("/readyz = %d %q\nwant 503 carrying the command %q", code, body, want)
	}
}
