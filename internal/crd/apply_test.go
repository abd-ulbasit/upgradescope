package crd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"
)

const crdName = "clusterreadinesses.upgradescope.dev"

// establishOnCreate makes the fake behave like a real apiserver: a created
// CRD immediately reports Established=True (mutate-then-fall-through reactor).
func establishOnCreate(fc *apiextfake.Clientset) {
	fc.PrependReactor("create", "customresourcedefinitions",
		func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
			obj := action.(k8stesting.CreateAction).GetObject().(*apiextensionsv1.CustomResourceDefinition)
			obj.Status.Conditions = append(obj.Status.Conditions, apiextensionsv1.CustomResourceDefinitionCondition{
				Type: apiextensionsv1.Established, Status: apiextensionsv1.ConditionTrue,
			})
			return false, nil, nil // fall through to the default tracker reactor
		})
}

func parseManifest(t *testing.T) apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	var c apiextensionsv1.CustomResourceDefinition
	if err := yaml.UnmarshalStrict(Manifest, &c); err != nil {
		t.Fatalf("embedded manifest does not parse as a v1 CRD: %v", err)
	}
	return c
}

func TestManifestShape(t *testing.T) {
	c := parseManifest(t)
	if c.Name != crdName {
		t.Errorf("name = %q, want %q", c.Name, crdName)
	}
	if c.Spec.Group != "upgradescope.dev" {
		t.Errorf("group = %q, want upgradescope.dev", c.Spec.Group)
	}
	if c.Spec.Scope != apiextensionsv1.ClusterScoped {
		t.Errorf("scope = %q, want Cluster", c.Spec.Scope)
	}
	if c.Spec.Names.Plural != "clusterreadinesses" || c.Spec.Names.Kind != "ClusterReadiness" {
		t.Errorf("names = %+v, want plural clusterreadinesses kind ClusterReadiness", c.Spec.Names)
	}
	if len(c.Spec.Names.ShortNames) != 1 || c.Spec.Names.ShortNames[0] != "ucr" {
		t.Errorf("shortNames = %v, want [ucr]", c.Spec.Names.ShortNames)
	}
	if len(c.Spec.Names.Categories) != 1 || c.Spec.Names.Categories[0] != "upgradescope" {
		t.Errorf("categories = %v, want [upgradescope]", c.Spec.Names.Categories)
	}
	if len(c.Spec.Versions) != 1 || c.Spec.Versions[0].Name != "v1alpha1" {
		t.Fatalf("versions = %+v, want exactly v1alpha1", c.Spec.Versions)
	}
	v := c.Spec.Versions[0]
	if !v.Served || !v.Storage {
		t.Error("v1alpha1 must be served and storage")
	}
	if v.Subresources == nil || v.Subresources.Status == nil {
		t.Error("status subresource not enabled")
	}
	cols := map[string]bool{}
	for _, pc := range v.AdditionalPrinterColumns {
		cols[pc.Name] = true
	}
	for _, want := range []string{"Target", "Score", "Ready", "Age"} {
		if !cols[want] {
			t.Errorf("printer column %q missing (have %v)", want, cols)
		}
	}
	// READY shows the verdict (ready/blocked/unknown), never a bare
	// ready bool that would read True for an unassessed cluster.
	for _, pc := range v.AdditionalPrinterColumns {
		if pc.Name == "Ready" && (pc.JSONPath != ".status.targets[0].verdict" || pc.Type != "string") {
			t.Errorf("Ready column = %s (%s), want .status.targets[0].verdict (string)", pc.JSONPath, pc.Type)
		}
	}
	targets := v.Schema.OpenAPIV3Schema.Properties["status"].Properties["targets"].Items.Schema
	verdict, ok := targets.Properties["verdict"]
	if !ok || verdict.Type != "string" || len(verdict.Enum) != 3 {
		t.Errorf("status.targets[].verdict schema = %+v, want string enum of 3", verdict)
	}
	if v.Schema == nil || v.Schema.OpenAPIV3Schema == nil {
		t.Fatal("openAPIV3Schema missing")
	}
	if _, ok := v.Schema.OpenAPIV3Schema.Properties["status"]; !ok {
		t.Error("schema has no status property")
	}
}

func TestEnsureCRDCreates(t *testing.T) {
	ctx := context.Background()
	fc := apiextfake.NewClientset()
	establishOnCreate(fc)
	if err := EnsureCRD(ctx, fc); err != nil {
		t.Fatalf("EnsureCRD: %v", err)
	}
	got, err := fc.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, crdName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("CRD not created: %v", err)
	}
	if got.Spec.Scope != apiextensionsv1.ClusterScoped {
		t.Errorf("created CRD scope = %q, want Cluster", got.Spec.Scope)
	}
}

func TestEnsureCRDUpdatesExisting(t *testing.T) {
	ctx := context.Background()
	fc := apiextfake.NewClientset()
	establishOnCreate(fc)
	if err := EnsureCRD(ctx, fc); err != nil {
		t.Fatalf("first EnsureCRD: %v", err)
	}
	// Simulate drift: wipe printer columns on the stored object.
	got, err := fc.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, crdName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got.Spec.Versions[0].AdditionalPrinterColumns = nil
	if _, err := fc.ApiextensionsV1().CustomResourceDefinitions().Update(ctx, got, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	// Second EnsureCRD must take the AlreadyExists → update path and reconcile.
	if err := EnsureCRD(ctx, fc); err != nil {
		t.Fatalf("second EnsureCRD: %v", err)
	}
	got2, err := fc.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, crdName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got2.Spec.Versions[0].AdditionalPrinterColumns) == 0 {
		t.Error("EnsureCRD did not restore printer columns on the update path")
	}
}

func TestIsEstablished(t *testing.T) {
	var c apiextensionsv1.CustomResourceDefinition
	if isEstablished(&c) {
		t.Error("no conditions: want not established")
	}
	c.Status.Conditions = []apiextensionsv1.CustomResourceDefinitionCondition{
		{Type: apiextensionsv1.NamesAccepted, Status: apiextensionsv1.ConditionTrue},
		{Type: apiextensionsv1.Established, Status: apiextensionsv1.ConditionFalse},
	}
	if isEstablished(&c) {
		t.Error("Established=False: want not established")
	}
	c.Status.Conditions[1].Status = apiextensionsv1.ConditionTrue
	if !isEstablished(&c) {
		t.Error("Established=True: want established")
	}
}

func TestEnsureCRDCreateTimesOutWhenNeverEstablished(t *testing.T) {
	oldTimeout, oldInterval := establishTimeout, establishPollInterval
	establishTimeout, establishPollInterval = 150*time.Millisecond, 20*time.Millisecond
	defer func() { establishTimeout, establishPollInterval = oldTimeout, oldInterval }()

	fc := apiextfake.NewClientset() // fake never sets conditions
	err := EnsureCRD(context.Background(), fc)
	if err == nil || !strings.Contains(err.Error(), "Established") {
		t.Fatalf("err = %v, want not-Established timeout error", err)
	}
	// The CRD itself was still created; only establishment timed out.
	if _, gerr := fc.ApiextensionsV1().CustomResourceDefinitions().Get(
		context.Background(), crdName, metav1.GetOptions{}); gerr != nil {
		t.Errorf("CRD not created despite establishment timeout: %v", gerr)
	}
}

// existingCRD returns the embedded manifest as the apiserver would store it
// (defaulted, Established), so a test can pre-create the CRD out of band.
func existingCRD(t *testing.T) *apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	c := parseManifest(t)
	apiextensionsv1.SetObjectDefaults_CustomResourceDefinition(&c)
	c.Status.Conditions = []apiextensionsv1.CustomResourceDefinitionCondition{
		{Type: apiextensionsv1.Established, Status: apiextensionsv1.ConditionTrue},
	}
	return &c
}

// writeVerbs lists the mutating actions a fake recorded, as "verb[/patchType]".
func writeVerbs(fc *apiextfake.Clientset) []string {
	var out []string
	for _, a := range fc.Actions() {
		switch act := a.(type) {
		case k8stesting.PatchAction:
			out = append(out, "patch/"+string(act.GetPatchType()))
		case k8stesting.CreateAction, k8stesting.UpdateAction, k8stesting.DeleteAction:
			out = append(out, a.GetVerb())
		}
	}
	return out
}

// TestEnsureCRDPreservesForeignMetadata: a CRD installed by Argo CD or
// kubectl apply carries labels and annotations the agent does not own.
// Reconciling a drifted schema must keep them (a full Update dropped them
// on every agent start, so GitOps tools saw permanent drift).
func TestEnsureCRDPreservesForeignMetadata(t *testing.T) {
	ctx := context.Background()
	pre := existingCRD(t)
	pre.Labels = map[string]string{"app.kubernetes.io/instance": "upgradescope"}
	pre.Annotations = map[string]string{
		"argocd.argoproj.io/tracking-id":                   "upgradescope:apiextensions.k8s.io/CustomResourceDefinition:/" + crdName,
		"kubectl.kubernetes.io/last-applied-configuration": "{}",
	}
	pre.Spec.Versions[0].AdditionalPrinterColumns = nil // schema drift to repair
	fc := apiextfake.NewClientset(pre)

	if err := EnsureCRD(ctx, fc); err != nil {
		t.Fatalf("EnsureCRD: %v", err)
	}
	got, err := fc.ApiextensionsV1().CustomResourceDefinitions().Get(ctx, crdName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Labels["app.kubernetes.io/instance"] != "upgradescope" {
		t.Errorf("labels = %v, want the GitOps instance label kept", got.Labels)
	}
	for k := range pre.Annotations {
		if _, ok := got.Annotations[k]; !ok {
			t.Errorf("annotation %q dropped (annotations = %v)", k, got.Annotations)
		}
	}
	if len(got.Spec.Versions[0].AdditionalPrinterColumns) == 0 {
		t.Error("schema drift not repaired: printer columns still missing")
	}
	if w := writeVerbs(fc); len(w) != 1 || w[0] != "patch/application/apply-patch+yaml" {
		t.Errorf("writes = %v, want exactly one server-side apply patch", w)
	}
}

// TestEnsureCRDNoWriteWhenInSync: an agent restart against a CRD that
// already matches the embedded manifest must not write at all.
func TestEnsureCRDNoWriteWhenInSync(t *testing.T) {
	fc := apiextfake.NewClientset(existingCRD(t))
	if err := EnsureCRD(context.Background(), fc); err != nil {
		t.Fatalf("EnsureCRD: %v", err)
	}
	if w := writeVerbs(fc); len(w) != 0 {
		t.Errorf("writes = %v, want none for an in-sync CRD", w)
	}
}

// TestEnsureCRDMissingAndCreateForbidden: the chart grants no CRD create
// (crds/ installs it). A missing CRD must surface as ErrCRDNotInstalled
// with a message that says how to install it, not a bare Forbidden.
func TestEnsureCRDMissingAndCreateForbidden(t *testing.T) {
	fc := apiextfake.NewClientset()
	fc.PrependReactor("create", "customresourcedefinitions",
		func(k8stesting.Action) (bool, k8sruntime.Object, error) {
			return true, nil, apierrors.NewForbidden(
				schema.GroupResource{Group: "apiextensions.k8s.io", Resource: "customresourcedefinitions"}, crdName, errors.New("RBAC"))
		})
	err := EnsureCRD(context.Background(), fc)
	if !errors.Is(err, ErrCRDNotInstalled) {
		t.Fatalf("err = %v, want ErrCRDNotInstalled", err)
	}
	if !strings.Contains(err.Error(), "crds/") {
		t.Errorf("err = %q, want it to point at the chart's crds/", err)
	}
}
