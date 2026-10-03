package apigroup

import "fmt"

// The group v0.1.x and the v0.2.0 release candidates used, on a domain
// the project never owned (#68). Objects annotated for those releases
// keep working for one minor release: the scanner reads the old ignore
// keys through ReadIgnore and says they are deprecated. It never writes
// them. v0.3.0 drops these constants and the reads.
const (
	LegacyGroup                  = "upgradescope.dev"
	LegacyAnnotationPrefix       = LegacyGroup + "/"
	LegacyIgnoreAnnotation       = LegacyAnnotationPrefix + "ignore"
	LegacyIgnoreReasonAnnotation = LegacyAnnotationPrefix + "ignore-reason"
)

// ReadIgnore returns an object's ignore and ignore-reason annotation
// values, reading each current key and, when the current one is absent,
// its pre-v0.2.0 key: the current key wins when both are set, even set
// empty. legacy reports that a value came from an old key. get returns an
// annotation's value and whether the object has it.
func ReadIgnore(get func(key string) (string, bool)) (ignore, reason string, legacy bool) {
	ignore, oldIgnore := read(get, IgnoreAnnotation, LegacyIgnoreAnnotation)
	reason, oldReason := read(get, IgnoreReasonAnnotation, LegacyIgnoreReasonAnnotation)
	return ignore, reason, oldIgnore || oldReason
}

func read(get func(string) (string, bool), current, old string) (value string, legacy bool) {
	if v, ok := get(current); ok {
		return v, false
	}
	v, ok := get(old)
	return v, ok
}

// LegacyIgnoreWarning is the deprecation notice for an object (named as
// the caller names objects) whose ignore annotations use the old keys.
func LegacyIgnoreWarning(object string) string {
	return fmt.Sprintf("object %s: annotation keys %s and %s are deprecated and read only until v0.3.0: rename them to %s and %s",
		object, LegacyIgnoreAnnotation, LegacyIgnoreReasonAnnotation, IgnoreAnnotation, IgnoreReasonAnnotation)
}
