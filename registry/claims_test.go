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
	} {
		if got := matchersOverlap(tc.a, tc.b); got != tc.want {
			t.Errorf("matchersOverlap(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}
