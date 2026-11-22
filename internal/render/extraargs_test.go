package render

import (
	"strings"
	"testing"
)

// TestExtraArgsAreIsolatedPerComponent 覆盖真机踩到的坑：FlagsData 必须按
// 组件隔离，否则 kubelet 与 etcd 复用同一份数据后会拿到 apiserver 专属
// flag（例如 --audit-log-format），启动即失败；同时自己的 extraArgs 被忽略。
func TestExtraArgsAreIsolatedPerComponent(t *testing.T) {
	cluster := clusterFor("staticpod")
	cluster.ControlPlane.APIServer.ExtraArgs = map[string]string{
		"audit-log-format": "json",
	}
	cluster.Node.Kubelet.ExtraArgs = map[string]string{
		"max-pods": "77",
	}
	cluster.ControlPlane.Etcd.Local.ExtraArgs = map[string]string{
		"log-level": "debug",
	}
	c := builderContext(t, cluster, "m1")
	spec := renderAll(t, c)

	api := string(mustFind(t, spec, c.Paths.Resolve("/etc/systemd/system/kube-apiserver.service")).Content)
	kubeletUnit := string(mustFind(t, spec, c.Paths.Resolve("/etc/systemd/system/kubelet.service")).Content)
	etcd := string(mustFind(t, spec, c.Paths.Resolve("/etc/systemd/system/etcd.service")).Content)

	if !strings.Contains(api, "--audit-log-format=json") {
		t.Errorf("apiserver 缺少自己的 extraArgs:\n%s", api)
	}
	if !strings.Contains(kubeletUnit, "--max-pods=77") {
		t.Errorf("kubelet 缺少自己的 extraArgs:\n%s", kubeletUnit)
	}
	if strings.Contains(kubeletUnit, "audit-log-format") {
		t.Errorf("kubelet 不应继承 apiserver 的 extraArgs:\n%s", kubeletUnit)
	}
	if !strings.Contains(etcd, "--log-level=debug") {
		t.Errorf("etcd 缺少自己的 extraArgs:\n%s", etcd)
	}
	if strings.Contains(etcd, "audit-log-format") {
		t.Errorf("etcd 不应继承 apiserver 的 extraArgs:\n%s", etcd)
	}
	if strings.Contains(api, "max-pods") {
		t.Errorf("apiserver 不应继承 kubelet 的 extraArgs:\n%s", api)
	}
}
