// Package deps pins dependencies used by upcoming packages so parallel
// implementation tracks never modify go.mod. Deleted when P1 lands.
package deps

import (
	_ "github.com/Masterminds/semver/v3"
	_ "github.com/prometheus/common/expfmt"
	_ "github.com/spf13/cobra"
	_ "k8s.io/client-go/kubernetes"
	_ "k8s.io/client-go/metadata"
	_ "sigs.k8s.io/yaml"
)
