package etcd

import (
	"slices"
	"strings"
	"testing"

	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
)

func testCluster(external bool) *clusterconfig.Cluster {
	cluster := &clusterconfig.Cluster{
		Cluster: clusterconfig.ClusterSpec{
			Hosts: clusterconfig.Hosts{
				{Name: "m1", Address: "10.0.0.1", Roles: []clusterconfig.Role{clusterconfig.RoleMaster}},
				{Name: "m2", Address: "10.0.0.2", Roles: []clusterconfig.Role{clusterconfig.RoleMaster}},
				{Name: "w1", Address: "10.0.0.3", Roles: []clusterconfig.Role{clusterconfig.RoleWorker}},
			},
		},
		ControlPlane: clusterconfig.ControlPlaneSpec{
			Etcd: func() clusterconfig.EtcdSpec {
				spec := clusterconfig.EtcdSpec{Local: clusterconfig.LocalEtcdSpec{DataDir: constants.EtcdDataDir}}
				if external {
					spec.External = clusterconfig.ExternalEtcdSpec{
						Endpoints: []string{"https://etcd.example:2379"},
						CAFile:    "/usr/local/kubemono/etcd/ca.crt",
						CertFile:  "/usr/local/kubemono/etcd/client.crt",
						KeyFile:   "/usr/local/kubemono/etcd/client.key",
					}
				}
				return spec
			}(),
		},
	}
	return cluster
}

func hostNamed(t *testing.T, cluster *clusterconfig.Cluster, name string) *clusterconfig.Host {
	t.Helper()
	h, ok := cluster.Cluster.Hosts.FindHost(name)
	if !ok {
		t.Fatalf("host %s not found", name)
	}
	return h
}

func TestProviderFormMatrix(t *testing.T) {
	paths := constants.NewPaths("")
	cases := []struct {
		name       string
		cluster    *clusterconfig.Cluster
		host       string
		wantForm   Form
		wantMember bool
	}{
		{"stacked master", testCluster(false), "m1", FormStacked, true},
		{"stacked worker", testCluster(false), "w1", FormStacked, false},
		{"external unmanaged master", testCluster(true), "m1", FormExternalUnmanaged, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prov, err := NewProvider(tc.cluster, hostNamed(t, tc.cluster, tc.host), paths)
			if err != nil {
				t.Fatalf("NewProvider: %v", err)
			}
			if prov.Form() != tc.wantForm {
				t.Fatalf("Form = %s, want %s", prov.Form(), tc.wantForm)
			}
			local := prov.Local()
			if local.Member != tc.wantMember {
				t.Errorf("Local().Member = %v, want %v", local.Member, tc.wantMember)
			}
			if local.DataDir != constants.EtcdDataDir {
				t.Errorf("Local().DataDir = %s", local.DataDir)
			}
		})
	}
}

func TestProviderEndpointsAndLocalMember(t *testing.T) {
	paths := constants.NewPaths("")
	cluster := testCluster(false)
	cluster.ControlPlane.Etcd.External = clusterconfig.ExternalEtcdSpec{}
	prov, err := NewProvider(cluster, hostNamed(t, cluster, "m1"), paths)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := prov.ClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	// 本机 etcd 成员的 ClientConfig 以 127.0.0.1 开头，随后是全部 master。
	if cfg.Endpoints[0] != "https://127.0.0.1:2379" {
		t.Fatalf("ClientConfig endpoints = %v", cfg.Endpoints)
	}
	if !contains(cfg.Endpoints, "https://10.0.0.2:2379") {
		t.Fatalf("ClientConfig 缺少其他 master：%v", cfg.Endpoints)
	}
	if cfg.TLS.CAFile != paths.EtcdCertPath(constants.CACertName) ||
		cfg.TLS.CertFile != paths.EtcdCertPath(constants.EtcdKubemonoClientCertName) ||
		cfg.TLS.KeyFile != paths.EtcdCertPath(constants.EtcdKubemonoClientKeyName) {
		t.Fatalf("stacked 必须使用 pki/etcd/kubemono-client.*：%+v", cfg.TLS)
	}
	memberCfg, err := prov.MemberConfig()
	if err != nil {
		t.Fatal(err)
	}
	for _, ep := range memberCfg.Endpoints {
		if strings.Contains(ep, "10.0.0.1") {
			t.Fatalf("MemberConfig 必须排除本机 endpoint：%v", memberCfg.Endpoints)
		}
	}
	if got := prov.ConfiguredNames(); strings.Join(got, ",") != "m1,m2" {
		t.Fatalf("ConfiguredNames = %v", got)
	}
	if local := prov.Local(); local.Name != "m1" || local.ClientURL != "https://10.0.0.1:2379" ||
		len(local.PeerURLs) != 1 || local.PeerURLs[0] != "https://10.0.0.1:2380" || local.ServiceName != ServiceName {
		t.Fatalf("Local = %+v", local)
	}
}

func TestProviderSingleMemberMemberConfigFallsBackToLocal(t *testing.T) {
	cluster := testCluster(false)
	cluster.Cluster.Hosts = []clusterconfig.Host{{Name: "m1", Address: "10.0.0.1", Roles: []clusterconfig.Role{clusterconfig.RoleMaster}}}
	prov, err := NewProvider(cluster, hostNamed(t, cluster, "m1"), constants.NewPaths(""))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := prov.MemberConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Endpoints) != 1 || cfg.Endpoints[0] != "https://127.0.0.1:2379" {
		t.Fatalf("单节点成员操作必须落回本机：%v", cfg.Endpoints)
	}
}

func TestProviderUnmanagedUsesExternalTLSAndRejectsMissingFiles(t *testing.T) {
	cluster := testCluster(true)
	prov, err := NewProvider(cluster, hostNamed(t, cluster, "m1"), constants.NewPaths(""))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := prov.ClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Endpoints) != 1 || cfg.Endpoints[0] != "https://etcd.example:2379" {
		t.Fatalf("endpoints = %v", cfg.Endpoints)
	}
	if cfg.TLS.CAFile != "/usr/local/kubemono/etcd/ca.crt" {
		t.Fatalf("unmanaged 必须使用用户提供的证书：%+v", cfg.TLS)
	}
	if prov.ConfiguredNames() != nil {
		t.Fatal("unmanaged 的成员表不在 kubemono 配置里")
	}
	// 缺少证书文件时构造参数必须报错。
	cluster.ControlPlane.Etcd.External = clusterconfig.ExternalEtcdSpec{Endpoints: []string{"https://etcd.example:2379"}}
	if _, err := prov.ClientConfig(); err == nil {
		t.Fatal("缺 caFile/certFile/keyFile 必须报错")
	}
}

func TestProviderErrors(t *testing.T) {
	if _, err := NewProvider(nil, nil, constants.NewPaths("")); err == nil {
		t.Fatal("缺集群配置必须报错")
	}
	// host 为 nil 时只读参数仍可用（准备机上执行 etcd health）。
	stacked := testCluster(false)
	stacked.ControlPlane.Etcd.External = clusterconfig.ExternalEtcdSpec{}
	prov, err := NewProvider(stacked, nil, constants.NewPaths(""))
	if err != nil {
		t.Fatal(err)
	}
	if prov.Local().Member {
		t.Fatal("无法识别本机身份时不应声称自己是 etcd 成员")
	}
	cfg, err := prov.ClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Endpoints) != 2 {
		t.Fatalf("endpoints = %v", cfg.Endpoints)
	}
}

func contains(list []string, want string) bool {
	return slices.Contains(list, want)
}

func TestProviderLocalUsesNodeName(t *testing.T) {
	cluster := testCluster(false)
	host := hostNamed(t, cluster, "m1")
	host.NodeName = clusterconfig.NodeNameFromAddress
	prov, err := NewProvider(cluster, host, constants.NewPaths(""), "10.0.0.1")
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if got := prov.Local().Name; got != "10.0.0.1" {
		t.Fatalf("Local().Name = %q, want 10.0.0.1", got)
	}
	if got := prov.ConfiguredNames(); len(got) == 0 || got[0] != "10.0.0.1" {
		t.Fatalf("ConfiguredNames = %v, want first 10.0.0.1", got)
	}
}
