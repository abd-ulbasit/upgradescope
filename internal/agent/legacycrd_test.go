package agent

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"

	"github.com/abd-ulbasit/upgradescope/internal/crd"
)

// An agent that manages the CRD and finds the pre-v0.2.0 one, on the old
// group, still installed says so once at startup, with the command that
// removes it. It installs the new CRD and creates the new object, and it
// never deletes the old CRD itself.
func TestRunWarnsOnceAboutTheLegacyCRD(t *testing.T) {
	apiext := fakeAPIExt()
	old := &apiextensionsv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{Name: crd.LegacyCRDName}}
	if _, err := apiext.ApiextensionsV1().CustomResourceDefinitions().Create(context.Background(), old, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	runOneTick(t, apiext, Config{Logger: slog.New(slog.NewJSONHandler(logs, nil))})

	warned := linesWithMsg(logs.lines(t), msgLegacyCRD)
	if len(warned) != 1 || warned[0]["level"] != "WARN" ||
		warned[0]["cleanup"] != "kubectl delete crd clusterreadinesses.upgradescope.dev" {
		t.Errorf("legacy CRD lines = %v, want one WARN line with the cleanup command", warned)
	}
	crds := apiext.ApiextensionsV1().CustomResourceDefinitions()
	if _, err := crds.Get(context.Background(), crd.LegacyCRDName, metav1.GetOptions{}); err != nil {
		t.Errorf("the agent removed the old CRD: %v", err)
	}
	if _, err := crds.Get(context.Background(), crd.CRDName, metav1.GetOptions{}); err != nil {
		t.Errorf("the agent did not install the new CRD: %v", err)
	}
	for _, a := range apiext.Actions() {
		if a.GetVerb() == "delete" {
			t.Errorf("apiextensions action %v: the agent must not delete a CRD", a)
		}
	}
}

// Without the old CRD there is nothing to say.
func TestRunNoLegacyCRDNoWarning(t *testing.T) {
	logs := &syncBuffer{}
	runOneTick(t, fakeAPIExt(), Config{Logger: slog.New(slog.NewJSONHandler(logs, nil))})
	if warned := linesWithMsg(logs.lines(t), msgLegacyCRD); len(warned) != 0 {
		t.Errorf("legacy CRD lines = %v, want none", warned)
	}
}

// The chart upgraded without installing the new CRD first (Helm does not
// install crds/ on upgrade): the agent may not create it and exits. With
// the old CRD still installed, the cause is the group move, so the error
// says that and where the migration steps are, not only that a CRD is
// missing.
func TestRunNamesTheGroupMoveWhenOnlyTheLegacyCRDIsInstalled(t *testing.T) {
	apiext := apiextfake.NewClientset(&apiextensionsv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{Name: crd.LegacyCRDName}})
	apiext.PrependReactor("create", "customresourcedefinitions",
		func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(
				schema.GroupResource{Group: "apiextensions.k8s.io", Resource: "customresourcedefinitions"}, "x", errors.New("RBAC"))
		})
	err := Run(context.Background(), fakeClients(t, "v1.35.2"), fakeDyn(), apiext, mustKB(t), Config{})
	if !errors.Is(err, crd.ErrCRDNotInstalled) {
		t.Fatalf("Run err = %v, want crd.ErrCRDNotInstalled", err)
	}
	for _, want := range []string{crd.LegacyCRDName, crd.Group, "helm upgrade", crd.UpgradeGuideURL} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Run err = %v\nwant it to name %q", err, want)
		}
	}
}
