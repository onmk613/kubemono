package kubelet

import (
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// TestSetAnnotation 固定注解重写的三件事：值用合法 YAML 编码（任意字符安全）、
// 行锚定用"缩进+键名"（前缀键不误伤）、缺少注解时报错。
func TestSetAnnotation(t *testing.T) {
	manifest := []byte(`apiVersion: v1
kind: Pod
metadata:
  annotations:
    kubemono.io/restarted-at: "2026-01-01T00:00:00Z"
    kubemono.io/restarted-at-extra: keep
`)
	out, err := setAnnotation(manifest, AnnotationRestartedAt, "2026-11-17T03:00:00Z")
	if err != nil {
		t.Fatalf("setAnnotation: %v", err)
	}
	s := string(out)
	if !strings.Contains(s, `kubemono.io/restarted-at: "2026-11-17T03:00:00Z"`) {
		t.Fatalf("注解值未更新:\n%s", s)
	}
	if strings.Contains(s, "2026-01-01") {
		t.Fatal("旧值仍在")
	}
	if !strings.Contains(s, "kubemono.io/restarted-at-extra: keep") {
		t.Fatalf("前缀相似的其他注解被误伤:\n%s", s)
	}
	// 结果必须是可解析的合法 YAML（kubelet 只认合法清单）。
	var doc map[string]any
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("重写后的清单不是合法 YAML: %v\n%s", err, out)
	}
	// 引号与冒号等特殊字符必须被 YAML 编码，解析回的注解值与写入值一致。
	out, err = setAnnotation(manifest, AnnotationRestartedAt, "a\"b: c\nd")
	if err != nil {
		t.Fatalf("setAnnotation 特殊字符: %v", err)
	}
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("特殊字符编码后不是合法 YAML: %v\n%s", err, out)
	}
	meta := doc["metadata"].(map[string]any)
	ann := meta["annotations"].(map[string]any)
	if got := ann[AnnotationRestartedAt]; got != "a\"b: c\nd" {
		t.Fatalf("特殊字符值解析不一致: %q", got)
	}
	if _, err := setAnnotation([]byte("kind: Pod\n"), AnnotationRestartedAt, "x"); err == nil {
		t.Fatal("缺少注解必须报错")
	}
}
