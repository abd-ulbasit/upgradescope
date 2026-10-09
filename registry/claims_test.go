package registry

import "testing"

func TestPathMatches(t *testing.T) {
	for _, tc := range []struct {
		path, matcher string
		want          bool
	}{
		// Two or more segments: a suffix on whole segments.
		{"ingress-nginx/controller", "ingress-nginx/controller", true},
		{"registry-k8s-io/ingress-nginx/controller", "ingress-nginx/controller", true},
		{"xingress-nginx/controller", "ingress-nginx/controller", false},
		// One segment: the repository exactly.
		{"etcd", "etcd", true},
		{"bitnami/etcd", "etcd", false},
		{"harbor.corp/k8s/controller", "controller", false},
		// "*/name": the final segment under any registry prefix, opted into.
		{"etcd", "*/etcd", true},
		{"library/etcd", "*/etcd", true},
		{"k8s/etcd", "*/etcd", true},
		{"bitnamilegacy/etcd", "*/etcd", true},
		{"a/b/c/etcd", "*/etcd", true},
		{"etcd-development/etcd", "*/etcd", true},
		{"my-etcd", "*/etcd", false},
		{"coreos/etcd-operator", "*/etcd", false},
		{"etcd/backup", "*/etcd", false},
	} {
		if got := PathMatches(tc.path, tc.matcher); got != tc.want {
			t.Errorf("PathMatches(%q, %q) = %v, want %v", tc.path, tc.matcher, got, tc.want)
		}
	}
}

func TestMatchersOverlap(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"*/etcd", "*/etcd", true},
		{"*/etcd", "etcd", true},
		{"*/etcd", "coreos/etcd", true},
		{"coreos/etcd", "*/etcd", true},
		{"*/etcd", "corp/mirror/etcd", true},
		{"*/etcd", "*/coredns", false},
		{"*/etcd", "etcd/backup", false},
		// A provider build is claimed by matchers naming the provider only.
		{"*/etcd", "mcr.microsoft.com/oss/etcd", false},
		// A tag-qualified matcher takes the tags it names ahead of a
		// path-only matcher of the same repository (#265), so the two never
		// claim one image; two tag-qualified matchers on one repository
		// are refused, whatever their patterns.
		{"rancher/nginx-ingress-controller:*-hardened*", "rancher/nginx-ingress-controller", false},
		{"rancher/nginx-ingress-controller", "rancher/nginx-ingress-controller:*-hardened*", false},
		{"rancher/nginx-ingress-controller:*-hardened*", "rancher/nginx-ingress-controller:*-rancher*", true},
		{"corp/rancher/nginx-ingress-controller:*-x*", "rancher/nginx-ingress-controller:*-hardened*", true},
		{"rancher/nginx-ingress-controller:*-hardened*", "acme/nginx-ingress-controller:*-hardened*", false},
	} {
		if got := matchersOverlap(tc.a, tc.b); got != tc.want {
			t.Errorf("matchersOverlap(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestSplitTagPattern(t *testing.T) {
	for _, tc := range []struct{ matcher, path, pattern string }{
		{"rancher/nginx-ingress-controller:*-hardened*", "rancher/nginx-ingress-controller", "*-hardened*"},
		{"rancher/nginx-ingress-controller", "rancher/nginx-ingress-controller", ""},
		{"*/etcd", "*/etcd", ""},
	} {
		if path, pattern := SplitTagPattern(tc.matcher); path != tc.path || pattern != tc.pattern {
			t.Errorf("SplitTagPattern(%q) = %q, %q; want %q, %q", tc.matcher, path, pattern, tc.path, tc.pattern)
		}
	}
}

func TestTagMatches(t *testing.T) {
	for _, tc := range []struct {
		tag, pattern string
		want         bool
	}{
		// RKE2's build: "-hardenedN" in both tag forms it has used.
		{"v1.12.6-hardened1", "*-hardened*", true},
		{"nginx-1.9.4-hardened1", "*-hardened*", true},
		// RKE1's build of the same repository: "-rancherN".
		{"nginx-1.12.1-rancher4", "*-hardened*", false},
		{"0.21.0-rancher1", "*-hardened*", false},
		{"", "*-hardened*", false},
		{"v1.12.6-hardened1", "v1.12.6-hardened1", true},
	} {
		if got := TagMatches(tc.tag, tc.pattern); got != tc.want {
			t.Errorf("TagMatches(%q, %q) = %v, want %v", tc.tag, tc.pattern, got, tc.want)
		}
	}
}

func TestComponentProductLine(t *testing.T) {
	c := ComponentImage{Image: "fluxcd/helm-controller", Lines: []ComponentLine{
		{Component: "0.37", Product: "2.2"}, {Component: "1.0", Product: "2.3"}, {Component: "1.2", Product: "2.5"},
	}}
	for _, tc := range []struct{ version, want string }{
		{"1.2.0", "2.5"},
		{"0.37.4", "2.2"},
		{"1.0.1", "2.3"},
		{"1.1.0", ""}, // a line the entry does not map: no version, never a guess
		{"1.2", "2.5"},
		{"1.2-rc.1", "2.5"}, // a pre-release is its line's (versionFromTag keeps "-rc.1")
		{"1.0-beta.2", "2.3"},
		{"1", ""},
		{"", ""},
	} {
		if got := c.ProductLine(tc.version); got != tc.want {
			t.Errorf("ProductLine(%q) = %q, want %q", tc.version, got, tc.want)
		}
	}
}
