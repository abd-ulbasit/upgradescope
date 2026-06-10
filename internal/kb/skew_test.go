package kb

import "testing"

func TestDefaultSkewPolicy(t *testing.T) {
	got := DefaultSkewPolicy()
	want := SkewPolicy{
		KubeletMaxBehind:   3,
		KubectlMaxSkew:     1,
		APIServerHASpread:  1,
		KubeProxyMaxBehind: 3,
		CtrlMgrMaxBehind:   1,
	}
	if got != want {
		t.Errorf("DefaultSkewPolicy() = %+v, want %+v", got, want)
	}
}
