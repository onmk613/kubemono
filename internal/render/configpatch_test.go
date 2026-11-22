package render

import (
	"testing"

	"sigs.k8s.io/yaml"
)

func TestMergeConfigPatchDeepMerges(t *testing.T) {
	rendered := "apiVersion: kubelet.config.k8s.io/v1beta1\nkind: KubeletConfiguration\nmaxPods: 100\nevictionHard:\n  memory.available: 100Mi\n"
	got, err := MergeConfigPatch(rendered, map[string]any{
		"maxPods": float64(200),
		"evictionHard": map[string]any{
			"nodefs.available": "10%",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := yaml.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["maxPods"] != float64(200) {
		t.Fatalf("maxPods = %v, want 200", parsed["maxPods"])
	}
	eviction, _ := parsed["evictionHard"].(map[string]any)
	if eviction["memory.available"] != "100Mi" {
		t.Fatalf("已有嵌套字段被覆盖: %v", eviction)
	}
	if eviction["nodefs.available"] != "10%" {
		t.Fatalf("patch 嵌套字段未生效: %v", eviction)
	}
}
