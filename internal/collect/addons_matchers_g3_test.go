package collect

import (
	"testing"

	"github.com/abd-ulbasit/upgradescope/registry"
)

// Image paths of the cluster-operations and storage add-ons, as the upstream
// project, AWS (ECR Public, the EKS add-on's private ECR repository) and the
// distributions publish them. Repositories were read on 2026-10-12 (Docker
// Hub's tags API; the projects' install manifests and image tables). Each tag
// must read back as the upstream version.
func TestMatchAddOnsStorageAndOperationsImagePaths(t *testing.T) {
	addons, err := registry.Load()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		image, wantID, wantVersion string
	}{
		{"registry.k8s.io/autoscaling/cluster-autoscaler:v1.35.0", "cluster-autoscaler", "1.35.0"},
		{"k8s.gcr.io/autoscaling/cluster-autoscaler:v1.17.3", "cluster-autoscaler", "1.17.3"},
		{"k8s.gcr.io/cluster-autoscaler:v1.14.7", "cluster-autoscaler", "1.14.7"},
		{"docker.io/bitnamilegacy/cluster-autoscaler:1.33.0-debian-12-r5", "cluster-autoscaler", "1.33.0"},

		{"registry.k8s.io/provider-aws/aws-ebs-csi-driver:v1.66.1", "aws-ebs-csi-driver", "1.66.1"},
		{"public.ecr.aws/ebs-csi-driver/aws-ebs-csi-driver:v1.66.1", "aws-ebs-csi-driver", "1.66.1"},
		{"602401143452.dkr.ecr.us-west-2.amazonaws.com/eks/aws-ebs-csi-driver:v1.35.0-eksbuild.2", "aws-ebs-csi-driver", "1.35.0"},

		{"public.ecr.aws/efs-csi-driver/amazon/aws-efs-csi-driver:v3.5.1", "aws-efs-csi-driver", "3.5.1"},
		{"602401143452.dkr.ecr.us-west-2.amazonaws.com/eks/aws-efs-csi-driver:v2.1.0-eksbuild.1", "aws-efs-csi-driver", "2.1.0"},
		{"docker.io/amazon/aws-efs-csi-driver:v1.7.0", "aws-efs-csi-driver", "1.7.0"},

		{"docker.io/longhornio/longhorn-manager:v1.13.0", "longhorn", "1.13.0"},
		{"longhornio/longhorn-ui:v1.10.0", "longhorn", "1.10.0"},
		{"docker.io/rancher/mirrored-longhornio-longhorn-manager:v1.12.1", "longhorn", "1.12.1"},

		// Components that version on their own stay unmatched: a sidecar's
		// v4.9.0 is not the driver's or Longhorn's.
		{"registry.k8s.io/sig-storage/csi-provisioner:v5.3.0", "", ""},
		{"public.ecr.aws/eks-distro/kubernetes-csi/external-provisioner:v5.3.0-eks-1-33-4", "", ""},
		{"docker.io/longhornio/csi-attacher:v4.9.0-20250826", "", ""},
		{"docker.io/longhornio/longhorn-instance-manager:v1_20230617", "", ""},
		// Old engine-image DaemonSets outlive an upgrade until volumes are
		// upgraded; reading them would report a second, older Longhorn.
		{"longhornio/longhorn-engine:v1.5.0", "", ""},
		{"docker.io/rancher/mirrored-longhornio-longhorn-engine:v1.5.0", "", ""},
	}
	for _, tc := range cases {
		if ids := imageAddOns(parseImage(tc.image), addons); len(ids) > 1 {
			t.Errorf("%s: claimed by %v, want at most one entry", tc.image, ids)
		}
		got, unrec := matchAddOns(addOnEvidence{images: []nsImage{{"ns", tc.image}}}, addons)
		if tc.wantID == "" {
			if len(got) != 0 || len(unrec) != 1 {
				t.Errorf("%s: want unrecognized, got addons=%+v unrecognized=%v", tc.image, got, unrec)
			}
			continue
		}
		if len(got) != 1 || got[0].ID != tc.wantID || got[0].Version != tc.wantVersion {
			t.Errorf("%s: got %+v, want %s %q", tc.image, got, tc.wantID, tc.wantVersion)
		}
	}
}
