package bom

import (
	"strings"
	"testing"
)

func TestParseRejectsKubemonoComponent(t *testing.T) {
	const doc = `release: v1.37.1-km.1
kubernetes: v1.37.1
arch: [amd64]
components:
  kubelet:
    version: v1.37.1
    category: k8s
    binaries: [kubelet]
  kubemono:
    version: v1.37.1-km.1
    category: k8s
    binaries: [kubemono]
`
	_, err := Parse([]byte(doc))
	if err == nil || !strings.Contains(err.Error(), "kubemono") {
		t.Fatalf("BOM 包含 kubemono 时必须拒绝，实际 err=%v", err)
	}
}
