package apigroup

import (
	"fmt"
	"strings"
)

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
// empty. ignoreLegacy and reasonLegacy report, for each value apart, that
// it came from an old key: an object can carry the new ignore key with the
// old ignore-reason key. get returns an annotation's value and whether the
// object has it.
func ReadIgnore(get func(key string) (string, bool)) (ignore, reason string, ignoreLegacy, reasonLegacy bool) {
	ignore, ignoreLegacy = read(get, IgnoreAnnotation, LegacyIgnoreAnnotation)
	reason, reasonLegacy = read(get, IgnoreReasonAnnotation, LegacyIgnoreReasonAnnotation)
	return ignore, reason, ignoreLegacy, reasonLegacy
}

func read(get func(string) (string, bool), current, old string) (value string, legacy bool) {
	if v, ok := get(current); ok {
		return v, false
	}
	v, ok := get(old)
	return v, ok
}

// maxLegacyNamed bounds how many objects LegacyIgnoreWarning names.
const maxLegacyNamed = 5

// LegacyIgnoreWarning is the one deprecation notice for all the objects
// (named as the caller names objects, at least one) whose ignore
// annotations use the old keys. It names the first maxLegacyNamed and
// counts the rest, so a migrating cluster gets one bounded line however
// many objects carry the old keys: per-object lines would crowd the
// capability gaps out of a bounded list such as status.notAssessed.
func LegacyIgnoreWarning(objects []string) string {
	named, more := objects, ""
	if len(objects) > maxLegacyNamed {
		named, more = objects[:maxLegacyNamed], fmt.Sprintf(" and %d more", len(objects)-maxLegacyNamed)
	}
	noun := "objects"
	if len(objects) == 1 {
		noun = "object"
	}
	return fmt.Sprintf("annotation keys %s and %s are deprecated and read only until v0.3.0: rename them to %s and %s on %d %s: %s%s",
		LegacyIgnoreAnnotation, LegacyIgnoreReasonAnnotation, IgnoreAnnotation, IgnoreReasonAnnotation,
		len(objects), noun, strings.Join(named, ", "), more)
}
