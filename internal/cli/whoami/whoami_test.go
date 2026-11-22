package whoami_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"kubemono/internal/cli/clitest"
	clusterconfig "kubemono/internal/config"
	"kubemono/internal/identity"
)

// TestWhoamiReadsIdentity 覆盖 whoami 的主路径：cluster.yaml + node.yaml
// → 打印配置名、NodeName、角色与集群身份，且不写任何文件。
func TestWhoamiReadsIdentity(t *testing.T) {
	dir := t.TempDir()
	cluster := clusterconfig.DefaultCluster()
	cluster.Cluster.Hosts = []clusterconfig.Host{{
		Name:    "m1",
		Address: "10.0.0.1",
		Roles:   []clusterconfig.Role{clusterconfig.RoleMaster, clusterconfig.RoleWorker},
	}}
	clusterconfig.SetDefaults(cluster)
	data, err := clusterconfig.MarshalCluster(cluster)
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "cluster.yaml")
	if err := os.WriteFile(cfgPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	store := &identity.Store{Dir: dir}
	if err := store.Save(&clusterconfig.NodeConfig{
		ClusterID: "cid-1",
		HostName:  "m1",
		NodeName:  "m1",
		Address:   "10.0.0.1",
		Roles:     []clusterconfig.Role{clusterconfig.RoleMaster, clusterconfig.RoleWorker},
		Release:   "v1.37.1-km.1",
	}); err != nil {
		t.Fatal(err)
	}

	out, err := clitest.ExecuteRoot(t, "whoami", "-f", cfgPath, "--data-dir", dir, "--output", "json")
	if err != nil {
		t.Fatalf("whoami: %v\n%s", err, out)
	}
	var view struct {
		ConfigName        string   `json:"configName"`
		NodeName          string   `json:"nodeName"`
		InstalledNodeName string   `json:"installedNodeName"`
		Roles             []string `json:"roles"`
		Installed         bool     `json:"installed"`
		ClusterID         string   `json:"clusterID"`
		Release           string   `json:"release"`
	}
	if err := json.Unmarshal([]byte(out), &view); err != nil {
		t.Fatalf("输出不是合法 JSON: %v\n%s", err, out)
	}
	if view.ConfigName != "m1" || view.InstalledNodeName != "m1" {
		t.Fatalf("身份字段不正确: %+v", view)
	}
	if view.NodeName == "" {
		t.Fatalf("nodeName 不应为空: %+v", view)
	}
	if !view.Installed || view.ClusterID != "cid-1" || view.Release != "v1.37.1-km.1" {
		t.Fatalf("安装状态/集群身份不正确: %+v", view)
	}
	if len(view.Roles) != 2 {
		t.Fatalf("roles = %v", view.Roles)
	}
}
