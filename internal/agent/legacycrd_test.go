package agent

import (
	"context"
	"log/slog"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
