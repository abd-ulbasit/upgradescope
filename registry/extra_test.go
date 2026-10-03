package registry

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func writeEntry(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func ids(addons []AddOn) []string {
	var out []string
	for _, a := range addons {
		out = append(out, a.ID)
	}
	return out
}

func TestLoadExtra(t *testing.T) {
	t.Run("directory of entries", func(t *testing.T) {
		dir := t.TempDir()
		writeEntry(t, dir, "my-b.yaml", validYAML("my-b"))
		writeEntry(t, dir, "my-a.yaml", validYAML("my-a"))
		writeEntry(t, dir, "notes.txt", "ignored")
		got, err := LoadExtra(dir)
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"my-a", "my-b"}; !slices.Equal(ids(got), want) {
			t.Errorf("ids = %v, want %v", ids(got), want)
		}
	})
	t.Run("a single file", func(t *testing.T) {
		dir := t.TempDir()
		writeEntry(t, dir, "other.yaml", "not: [valid")
		p := writeEntry(t, dir, "my-a.yaml", validYAML("my-a"))
		got, err := LoadExtra(p)
		if err != nil {
			t.Fatalf("a file beside it must not be read: %v", err)
		}
		if want := []string{"my-a"}; !slices.Equal(ids(got), want) {
			t.Errorf("ids = %v, want %v", ids(got), want)
		}
	})
	// A mounted ConfigMap (the chart's agent.extraRegistry): the keys are
	// symlinks to ..data, itself a symlink to a timestamped directory.
	t.Run("a ConfigMap volume layout", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, "..2026_10_03_09_00_00.1"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeEntry(t, filepath.Join(dir, "..2026_10_03_09_00_00.1"), "my-a.yaml", validYAML("my-a"))
		for _, link := range [][2]string{
			{"..2026_10_03_09_00_00.1", "..data"},
			{"..data/my-a.yaml", "my-a.yaml"},
		} {
			if err := os.Symlink(link[0], filepath.Join(dir, link[1])); err != nil {
				t.Fatal(err)
			}
		}
		for _, path := range []string{dir, dir + string(filepath.Separator), filepath.Join(dir, "my-a.yaml")} {
			got, err := LoadExtra(path)
			if err != nil {
				t.Fatalf("LoadExtra(%q): %v", path, err)
			}
			if want := []string{"my-a"}; !slices.Equal(ids(got), want) {
				t.Errorf("LoadExtra(%q) ids = %v, want %v (the timestamped copy must not load twice)", path, ids(got), want)
			}
		}
	})
	// The same validator as the embedded entries: a file that fails it fails
	// the load and is named, in a directory and when given as the file.
	bad := strings.Replace(validYAML("my-a"), "https://vendor.dev/releases", "https://example.com/releases", 1)
	t.Run("an invalid entry names its file", func(t *testing.T) {
		dir := t.TempDir()
		writeEntry(t, dir, "my-ok.yaml", validYAML("my-ok"))
		writeEntry(t, dir, "my-a.yaml", bad)
		if _, err := LoadExtra(dir); err == nil || !strings.Contains(err.Error(), "my-a.yaml") || !strings.Contains(err.Error(), "example.com") {
			t.Errorf("want an error naming my-a.yaml and the placeholder citation, got %v", err)
		}
		if _, err := LoadExtra(filepath.Join(dir, "my-a.yaml")); err == nil || !strings.Contains(err.Error(), "my-a.yaml") {
			t.Errorf("file: want an error naming my-a.yaml, got %v", err)
		}
	})
	t.Run("a file name that is not the id", func(t *testing.T) {
		p := writeEntry(t, t.TempDir(), "mine.yaml", validYAML("my-a"))
		if _, err := LoadExtra(p); err == nil || !strings.Contains(err.Error(), "must equal the file name") {
			t.Errorf("got %v", err)
		}
	})
	t.Run("a .yml file is refused, not skipped", func(t *testing.T) {
		dir := t.TempDir()
		writeEntry(t, dir, "my-a.yml", validYAML("my-a"))
		if _, err := LoadExtra(dir); err == nil || !strings.Contains(err.Error(), ".yaml extension") {
			t.Errorf("got %v", err)
		}
	})
	// An empty or wrong path is a mistake (an unmounted ConfigMap), not
	// "no extra entries": loading none silently would leave add-ons unjudged.
	t.Run("nothing to load is an error", func(t *testing.T) {
		if _, err := LoadExtra(t.TempDir()); err == nil || !strings.Contains(err.Error(), "no *.yaml") {
			t.Errorf("empty directory: got %v", err)
		}
		if _, err := LoadExtra(filepath.Join(t.TempDir(), "missing")); err == nil {
			t.Error("missing path: want an error")
		}
		if _, err := LoadExtra(writeEntry(t, t.TempDir(), "notes.txt", "x")); err == nil {
			t.Error("a file that is not .yaml: want an error")
		}
	})
}

func TestMerge(t *testing.T) {
	base := []AddOn{{ID: "a", DisplayName: "base a"}, {ID: "c", DisplayName: "base c"}}
	extra := []AddOn{{ID: "b", DisplayName: "extra b"}, {ID: "c", DisplayName: "extra c"}}
	got := Merge(base, extra)
	if want := []string{"a", "b", "c"}; !slices.Equal(ids(got), want) {
		t.Fatalf("ids = %v, want %v", ids(got), want)
	}
	if got[2].DisplayName != "extra c" {
		t.Errorf("an extra entry with an embedded ID must replace it, got %q", got[2].DisplayName)
	}
	if base[1].DisplayName != "base c" {
		t.Error("Merge changed its base")
	}
}

// No image or chart is claimed by two entries: a second claim would judge
// one workload twice and, for an operator's entry, can raise a false blocker.
func TestClaimConflicts(t *testing.T) {
	entry := func(id string, images, charts []string) AddOn {
		return AddOn{ID: id, Matchers: Matchers{Images: images, Charts: charts}}
	}
	base, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if errs := ClaimConflicts(base); len(errs) != 0 {
		t.Fatalf("the embedded registry double-claims: %v", errs)
	}
	for _, tc := range []struct {
		name  string
		extra AddOn
		want  string // "" = no conflict
	}{
		{"same repository", entry("mine", []string{"cilium/operator"}, nil), `"cilium/operator"`},
		{"a longer mirror path of a claimed repository", entry("mine", []string{"corp/mirror/ingress-nginx/controller"}, nil), "ingress-nginx"},
		{"a one-segment matcher is exact, it cannot reach another product", entry("mine", []string{"operator"}, nil), ""},
		{"a one-segment matcher is exact, controller", entry("mine", []string{"controller"}, nil), ""},
		{"same chart", entry("mine", []string{"acme/thing"}, []string{"cert-manager"}), `chart "cert-manager"`},
		{"a provider build is not an upstream claim", entry("mine", []string{"mcr.microsoft.com/oss/calico/node"}, nil), ""},
		{"a replacement of the claiming entry itself", entry("cilium", []string{"cilium/operator"}, nil), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errs := ClaimConflicts(Merge(base, []AddOn{tc.extra}))
			if tc.want == "" {
				if len(errs) != 0 {
					t.Errorf("want no conflict, got %v", errs)
				}
				return
			}
			if len(errs) == 0 || !strings.Contains(errors.Join(errs...).Error(), tc.want) {
				t.Errorf("want a conflict mentioning %s, got %v", tc.want, errs)
			}
		})
	}
}
