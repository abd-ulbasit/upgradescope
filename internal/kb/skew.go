package kb

// SkewPolicy encodes the upstream Kubernetes version-skew policy
// (https://kubernetes.io/releases/version-skew-policy/). All values are
// minor-version distances relative to kube-apiserver.
//
// Extends the shared type contract with KubeProxyMaxBehind and
// CtrlMgrMaxBehind — both are upstream policy rules the engine evaluates.
type SkewPolicy struct {
	KubeletMaxBehind   int // kubelet ≤ apiserver and ≥ apiserver−3
	KubectlMaxSkew     int // kubectl within ±1 of apiserver
	APIServerHASpread  int // HA apiservers within 1 minor of each other
	KubeProxyMaxBehind int // kube-proxy ≤ apiserver and ≥ apiserver−3
	CtrlMgrMaxBehind   int // controller-manager/scheduler ≤ apiserver, ≥ apiserver−1
}

// DefaultSkewPolicy returns the upstream policy as of Kubernetes 1.28+
// (when kubelet skew widened from n−2 to n−3).
func DefaultSkewPolicy() SkewPolicy {
	return SkewPolicy{
		KubeletMaxBehind:   3,
		KubectlMaxSkew:     1,
		APIServerHASpread:  1,
		KubeProxyMaxBehind: 3,
		CtrlMgrMaxBehind:   1,
	}
}
