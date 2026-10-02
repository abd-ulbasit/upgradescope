package collect

import (
	"bytes"
	"errors"
	"io"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// kubectlObjects is the test oracle for parseManifestStream: it decodes s
// the way kubectl apply -f does — cli-runtime's StreamVisitor over
// apimachinery's YAMLOrJSONDecoder (4 KiB JSON peek), the unstructured JSON
// scheme, and FlattenListVisitor — and returns "group/version/Kind" for
// every object kubectl would send, in order. A decode error ends the
// stream (stopped is that error), as it ends kubectl's; a value that is not
// an object, or an object without kind or version, is skipped, as kubectl
// reports it and continues.
func kubectlObjects(s string) (gvks []string, stopped error) {
	d := utilyaml.NewYAMLOrJSONDecoder(strings.NewReader(s), 4096)
	for {
		var ext runtime.RawExtension
		if err := d.Decode(&ext); err != nil {
			if errors.Is(err, io.EOF) {
				return gvks, nil
			}
			return gvks, err
		}
		raw := bytes.TrimSpace(ext.Raw)
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		obj, _, err := unstructured.UnstructuredJSONScheme.Decode(raw, nil, nil)
		if err != nil {
			continue
		}
		queue := []runtime.Object{obj}
		for i := 0; i < len(queue); i++ {
			o := queue[i]
			if meta.IsListType(o) {
				items, err := meta.ExtractList(o)
				if err != nil {
					break
				}
				queue = append(queue, items...)
				continue
			}
			gvk := o.GetObjectKind().GroupVersionKind()
			if gvk.Kind == "" || gvk.Version == "" || gvk.Kind == "List" {
				continue // no resource to map it to: kubectl reports it
			}
			gvks = append(gvks, gvk.Group+"/"+gvk.Version+"/"+gvk.Kind)
		}
	}
}
