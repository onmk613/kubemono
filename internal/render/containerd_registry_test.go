package render

import (
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"

	clusterconfig "kubemono/internal/config"
)

func TestRenderRegistryHostsRespectsRoot(t *testing.T) {
	c := testContext("/tmp/kubemono-root")
	c.Cluster.Node.Runtime.Containerd.Registries.Configs = map[string]clusterconfig.RegistryAuthSpec{
		"registry.example.com": {},
	}
	hosts := RenderRegistryHosts(c)
	want := "/tmp/kubemono-root/etc/containerd/certs.d/registry.example.com/hosts.toml"
	if _, ok := hosts[want]; !ok {
		t.Fatalf("测试根下 hosts.toml 路径 = %v，期望包含 %s", keysOf(hosts), want)
	}
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestRenderRegistryHostsPutsMirrorsFirst 守住 containerd 的 host 顺序：
// [host] 按出现顺序尝试，server 只是兜底；上游不能排在 mirror 前面。
func TestRenderRegistryHostsPutsMirrorsFirst(t *testing.T) {
	c := testContext("")
	c.Cluster.Node.Runtime.Containerd.Registries.Mirrors = map[string][]string{
		"docker.io": {"https://mirror.example.com"},
	}
	hosts := RenderRegistryHosts(c)
	data := string(hosts[ContainerdCertsDir(c)+"/docker.io/hosts.toml"])
	mirror := strings.Index(data, `[host."https://mirror.example.com"]`)
	if mirror < 0 {
		t.Fatalf("缺少 mirror host:\n%s", data)
	}
	if upstream := strings.Index(data, `[host."https://docker.io"]`); upstream >= 0 && upstream < mirror {
		t.Fatalf("上游 host 不能排在 mirror 前:\n%s", data)
	}
	if !strings.Contains(data, `server = "https://docker.io"`) {
		t.Fatalf("没有上游认证时应保留 server 兜底:\n%s", data)
	}
}

// TestRenderRegistryHostsAuthFollowsHost：configs 的 key 是实际写入的 host；
// 上游认证挂在最后一个 host 上，mirror 认证挂在 mirror URL 上。
func TestRenderRegistryHostsAuthFollowsHost(t *testing.T) {
	c := testContext("")
	c.Cluster.Node.Runtime.Containerd.Registries.Mirrors = map[string][]string{
		"docker.io": {"https://mirror.example.com"},
	}
	c.Cluster.Node.Runtime.Containerd.Registries.Configs = map[string]clusterconfig.RegistryAuthSpec{
		"docker.io":          {Username: "up", Password: "pw"},
		"mirror.example.com": {Username: "m", Password: "p"},
	}
	hosts := RenderRegistryHosts(c)
	data := string(hosts[ContainerdCertsDir(c)+"/docker.io/hosts.toml"])
	if strings.Contains(data, "server =") {
		t.Fatalf("上游需要认证时不能再写 server（认证会丢）:\n%s", data)
	}
	if !strings.Contains(data, `[host."https://docker.io"]`) {
		t.Fatalf("上游认证必须挂在 host 上:\n%s", data)
	}
	if strings.Index(data, `[host."https://mirror.example.com"]`) > strings.Index(data, `[host."https://docker.io"]`) {
		t.Fatalf("mirror 必须先于带认证的上游 host:\n%s", data)
	}
}

// TestRenderRegistryHostsIsValidTOML：server 必须在任何 [host] 表之前，否则
// TOML 会把它解析成 host 表的键，根级 server 丢失。
func TestRenderRegistryHostsIsValidTOML(t *testing.T) {
	c := testContext("")
	c.Cluster.Node.Runtime.Containerd.Registries.Mirrors = map[string][]string{
		"docker.io": {"https://mirror.example.com"},
	}
	c.Cluster.Node.Runtime.Containerd.Registries.Configs = map[string]clusterconfig.RegistryAuthSpec{
		"mirror.example.com": {Username: "u", Password: "p"},
	}
	data := string(RenderRegistryHosts(c)[ContainerdCertsDir(c)+"/docker.io/hosts.toml"])
	var parsed map[string]any
	if err := toml.Unmarshal([]byte(data), &parsed); err != nil {
		t.Fatalf("hosts.toml 不是合法 TOML：%v\n%s", err, data)
	}
	if _, ok := parsed["server"]; !ok {
		t.Fatalf("根级 server 丢失：\n%s", data)
	}
	if _, ok := parsed["host"]; !ok {
		t.Fatalf("host 表丢失：\n%s", data)
	}
}
