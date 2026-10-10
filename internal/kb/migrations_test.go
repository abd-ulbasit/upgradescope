package kb

import (
	"strings"
	"testing"
)

func entriesForMigrations() []APILifecycleEntry {
	return []APILifecycleEntry{{Group: "policy", Version: "v1beta1", Kind: "PodSecurityPolicy"}, {Group: "", Version: "v1", Kind: "Pod"}}
}

// #330: data/migrations.json is merged onto the lifecycle entries by Load,
// and a file that cannot be trusted fails the load rather than dropping a
// note: a key with no entry, a duplicate, an empty note, a note without an
// https citation.
func TestApplyMigrationsRefusesAnUntrustworthyFile(t *testing.T) {
	good := `{"group":"policy","version":"v1beta1","kind":"PodSecurityPolicy","note":"use Pod Security Admission","citations":["https://example.com/a"]}`
	for name, tc := range map[string]struct{ file, want string }{
		"unknown key":   {`{"migrations":[{"group":"policy","version":"v1beta9","kind":"PodSecurityPolicy","note":"n","citations":["https://example.com/a"]}]}`, "not in apilifecycle.json"},
		"no citation":   {`{"migrations":[{"group":"policy","version":"v1beta1","kind":"PodSecurityPolicy","note":"n","citations":[]}]}`, "no https citation"},
		"http citation": {`{"migrations":[{"group":"policy","version":"v1beta1","kind":"PodSecurityPolicy","note":"n","citations":["http://example.com/a"]}]}`, "no https citation"},
		"empty note":    {`{"migrations":[{"group":"policy","version":"v1beta1","kind":"PodSecurityPolicy","note":" ","citations":["https://example.com/a"]}]}`, "empty"},
		"duplicate":     {`{"migrations":[` + good + `,` + good + `]}`, "two notes"},
		"corrupt":       {`{"migrations":`, "corrupt"},
		"valid":         {`{"migrations":[` + good + `]}`, ""},
	} {
		entries := entriesForMigrations()
		err := applyMigrations(entries, []byte(tc.file))
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: error %v, want one containing %q", name, err, tc.want)
		}
		if tc.want == "" && (entries[0].Migration == nil || entries[0].Migration.Note != "use Pod Security Admission") {
			t.Errorf("%s: the note was not merged: %+v", name, entries[0].Migration)
		}
	}
}

// The shipped notes load, every one is cited with https, and the lifecycle
// file itself carries none (gen-kb and kb-refresh never write them).
func TestShippedMigrationNotesAreCited(t *testing.T) {
	k, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range k.APILifecycle {
		if e.Migration == nil {
			continue
		}
		n++
		if !hasHTTPSCitation(e.Migration.Citations) || strings.TrimSpace(e.Migration.Note) == "" {
			t.Errorf("%s/%s %s: note %+v is not cited", e.Group, e.Version, e.Kind, e.Migration)
		}
	}
	if n < 10 {
		t.Errorf("only %d entries carry a note", n)
	}
	if strings.Contains(string(apilifecycleJSON), `"migration"`) {
		t.Error("apilifecycle.json holds a migration note: it is generated, and a refresh would drop it")
	}
}

// The lint: a kind removed by the KB's horizon that has no replacement the
// horizon serves and no served successor tells the user nothing to move
// to, so it MUST carry a note (#330). PodSecurityPolicy is the one the
// finding named; the lint catches the rest.
func TestRemovedKindWithoutSuccessorHasAMigrationNote(t *testing.T) {
	k, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	idx := NewIndex(k.APILifecycle)
	checked := 0
	for _, e := range k.APILifecycle {
		if e.Removed == nil || e.Removed.Compare(k.MaxKnownK8s) > 0 {
			continue
		}
		_, replaced := idx.ResolveReplacement(e, k.MaxKnownK8s)
		_, succeeded := idx.ServedSuccessor(e, k.MaxKnownK8s)
		if replaced || succeeded {
			continue
		}
		checked++
		if e.Migration == nil {
			t.Errorf("%s/%s %s is removed in %s with no replacement or served successor and no note in data/migrations.json", e.Group, e.Version, e.Kind, e.Removed)
		}
	}
	if checked < 10 {
		t.Fatalf("the lint matched only %d entries: it did not run", checked)
	}
	psp, _ := idx.Lookup("policy", "v1beta1", "PodSecurityPolicy")
	if psp.Migration == nil || !strings.Contains(psp.Migration.Note, "Pod Security Admission") {
		t.Errorf("PodSecurityPolicy note = %+v, want one naming Pod Security Admission", psp.Migration)
	}
}

// The schema-changing migrations the deprecation guide documents carry a
// note that names the changes, not just a new apiVersion.
func TestSchemaChangingMigrationsAreNoted(t *testing.T) {
	k, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	idx := NewIndex(k.APILifecycle)
	for _, c := range []struct{ group, version, kind, phrase string }{
		{"extensions", "v1beta1", "Ingress", "pathType"},
		{"networking.k8s.io", "v1beta1", "Ingress", "pathType"},
		{"apiextensions.k8s.io", "v1beta1", "CustomResourceDefinition", "structural"},
		{"admissionregistration.k8s.io", "v1beta1", "ValidatingWebhookConfiguration", "sideEffects"},
		{"admissionregistration.k8s.io", "v1beta1", "MutatingWebhookConfiguration", "admissionReviewVersions"},
		{"extensions", "v1beta1", "PodSecurityPolicy", "Pod Security Admission"},
	} {
		e, ok := idx.Lookup(c.group, c.version, c.kind)
		if !ok || e.Migration == nil || !strings.Contains(e.Migration.Note, c.phrase) {
			t.Errorf("%s/%s %s: note %+v does not name %q", c.group, c.version, c.kind, e.Migration, c.phrase)
		}
	}
}
