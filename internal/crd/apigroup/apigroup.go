// Package apigroup is the one place the project's Kubernetes API group is
// spelled: the ClusterReadiness CRD's group and the prefix of every
// annotation upgradescope reads or writes. It imports nothing, so every
// package can use it: internal/crd re-exports Group, and internal/collect
// and internal/suppress, which internal/crd itself imports, name the
// annotation keys from here.
package apigroup

const (
	// Group is the API group of the ClusterReadiness CRD.
	Group = "upgradescope.basit.engineer"
	// AnnotationPrefix starts every annotation key upgradescope reads or
	// writes.
	AnnotationPrefix = Group + "/"

	// IgnoreAnnotation lists, comma-separated, the finding categories or
	// keys an object accepts (see internal/suppress).
	IgnoreAnnotation = AnnotationPrefix + "ignore"
	// IgnoreReasonAnnotation says why; without it IgnoreAnnotation is not
	// applied.
	IgnoreReasonAnnotation = AnnotationPrefix + "ignore-reason"
	// StatusErrorAnnotation marks a ClusterReadiness whose status the agent
	// failed to write (see internal/crd).
	StatusErrorAnnotation = AnnotationPrefix + "status-error"
)
