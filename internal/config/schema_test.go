package config

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// TestLoadRejectsUnknownTopLevelFields 确认严格解析：未知顶层字段直接拒绝。
func TestLoadRejectsUnknownTopLevelFields(t *testing.T) {
	doc := `unexpected:
  name: test
other:
  mode: staticpod
  hosts:
    list:
      - {name: m1, address: 10.0.0.1, roles: [master]}
`
	if _, err := LoadCluster([]byte(doc)); err == nil {
		t.Fatal("未知顶层字段必须被拒绝")
	} else if !strings.Contains(err.Error(), "unexpected") && !strings.Contains(err.Error(), "other") {
		t.Fatalf("错误应指出未知字段，实际为: %v", err)
	}
}

// TestDefaultClusterUsesSixSections 固定根结构。
func TestDefaultClusterUsesSixSections(t *testing.T) {
	raw, err := MarshalCluster(DefaultCluster())
	if err != nil {
		t.Fatalf("MarshalCluster: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	for _, key := range []string{"cluster", "node", "controlPlane", "network", "addons", "images"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("默认配置缺少顶层字段 %q", key)
		}
	}
	for _, unexpected := range []string{"apiVersion", "kind", "metadata", "spec"} {
		if _, ok := doc[unexpected]; ok {
			t.Errorf("默认配置不应包含顶层字段 %q", unexpected)
		}
	}
}

func TestLoadRejectsKubernetesEnvelope(t *testing.T) {
	doc := `apiVersion: kubemono.io/v1beta1
kind: Cluster
cluster:
  hosts: [{name: m1, address: 10.0.0.1, roles: [master]}]
`
	if _, err := LoadCluster([]byte(doc)); err == nil {
		t.Fatal("cluster.yaml 不接受 apiVersion/kind 信封")
	}
}

func TestLoadRejectsLoadBalancerConfig(t *testing.T) {
	doc := `cluster:
  hosts:
    - {name: m1, address: 10.0.0.1, roles: [master]}
controlPlane:
  loadBalancer:
    localProxy: {enabled: true}
`
	if _, err := LoadCluster([]byte(doc)); err == nil {
		t.Fatal("loadBalancer 不是当前配置字段，必须被拒绝")
	} else if !strings.Contains(err.Error(), "loadBalancer") {
		t.Fatalf("错误应指出 loadBalancer 未知字段，实际为: %v", err)
	}
}

func TestLoadRejectsUnknownConfigFields(t *testing.T) {
	cases := map[string]string{
		"cluster name": `cluster:
  name: prod
  hosts: [{name: m1, address: 10.0.0.1, roles: [master]}]
`,
		"security": `cluster:
  hosts: [{name: m1, address: 10.0.0.1, roles: [master]}]
controlPlane:
  security: {encryptionAtRest: true}
`,
		"etcd type": `cluster:
  hosts: [{name: m1, address: 10.0.0.1, roles: [master]}]
controlPlane:
  etcd: {type: stacked}
`,
		"runtime type": `cluster:
  hosts: [{name: m1, address: 10.0.0.1, roles: [master]}]
node:
  runtime: {type: containerd}
`,
	}
	for field, doc := range cases {
		t.Run(field, func(t *testing.T) {
			if _, err := LoadCluster([]byte(doc)); err == nil {
				t.Fatalf("%s 不是当前配置字段，必须被拒绝", field)
			}
		})
	}
}
