package identity

import (
	"fmt"
	"net/netip"
	"strings"
	"testing"

	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
)

// fakes：测试绝不读取真实网卡，也不查看真实主机名。

// FakeLister 是一个由固定网卡列表支撑的 InterfaceLister。
type FakeLister struct {
	// Interfaces 在每次调用时返回。
	Interfaces []Interface
	// Err 非空时返回它而不是网卡列表。
	Err error
	// Calls 统计调用次数。
	Calls int
}

// List 实现 InterfaceLister。
func (f *FakeLister) List() ([]Interface, error) {
	f.Calls++
	if f.Err != nil {
		return nil, f.Err
	}
	return f.Interfaces, nil
}

// fakeHostname 返回一个固定值的 Hostname 函数。
func fakeHostname(name string) func() (string, error) {
	return func() (string, error) { return name, nil }
}

// iface 从紧凑的规格串构造一个假网卡：
func iface(t *testing.T, spec string) Interface {
	t.Helper()
	fields := strings.Fields(spec)
	if len(fields) == 0 {
		t.Fatalf("iface(%q): empty spec", spec)
	}
	out := Interface{Name: fields[0]}
	for _, f := range fields[1:] {
		switch f {
		case "up", "loopback", "pointtopoint", "multicast":
			out.Flags = append(out.Flags, f)
		case "ptp":
			out.Flags = append(out.Flags, FlagPointToPoint)
		default:
			p, err := netip.ParsePrefix(f)
			if err != nil {
				t.Fatalf("iface(%q): bad address %q: %v", spec, f, err)
			}
			out.Addresses = append(out.Addresses, p)
		}
	}
	return out
}

func ifaces(t *testing.T, specs ...string) []Interface {
	t.Helper()
	out := make([]Interface, 0, len(specs))
	for _, s := range specs {
		out = append(out, iface(t, s))
	}
	return out
}

// host 构造一条 hosts[].list 条目："name address role1,role2 extra1,extra2"。
func host(t *testing.T, spec string) clusterconfig.Host {
	t.Helper()
	fields := strings.Fields(spec)
	if len(fields) < 3 {
		t.Fatalf("host(%q): want at least name/address/roles", spec)
	}
	h := clusterconfig.Host{Name: fields[0], Address: fields[1]}
	for r := range strings.SplitSeq(fields[2], ",") {
		h.Roles = append(h.Roles, clusterconfig.Role(r))
	}
	if len(fields) > 3 && fields[3] != "-" {
		h.MatchAddresses = strings.Split(fields[3], ",")
	}
	return h
}

// testCluster 从主机规格串构造一个已填默认值的集群。
func testCluster(t *testing.T, hosts ...string) *clusterconfig.Cluster {
	t.Helper()
	c := &clusterconfig.Cluster{}
	for _, h := range hosts {
		c.Cluster.Hosts = append(c.Cluster.Hosts, host(t, h))
	}
	clusterconfig.SetDefaults(c)
	return c
}

// resolverFor 返回一个只接假输入的 resolver。
func resolverFor(lister *FakeLister, hostname, nodeConfigPath string) *Resolver {
	return &Resolver{
		Lister:         lister.List,
		Hostname:       fakeHostname(hostname),
		NodeConfigPath: nodeConfigPath,
	}
}

// missingNodeConfig 返回一个保证不存在的路径。
func missingNodeConfig(t *testing.T) string {
	t.Helper()
	return t.TempDir() + "/node.yaml"
}

func warningsContain(warnings []string, substr string) bool {
	for _, w := range warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}

// Resolve: --node（优先级 1）

func TestResolveOverride(t *testing.T) {
	cfg := testCluster(t,
		"m1 10.0.0.1 master",
		"w1 10.0.0.11 worker",
	)
	local := ifaces(t, "eth0 up 10.0.0.11/24", "lo up loopback 127.0.0.1/8")

	tests := []struct {
		name        string
		override    string
		force       bool
		wantNode    string
		wantAddress string
		wantErr     []string // 错误信息中必须出现的子串 / substrings that must appear in the error
		wantWarning []string
	}{
		{
			name:        "override hits a local address",
			override:    "w1",
			wantNode:    "w1",
			wantAddress: "10.0.0.11",
		},
		{
			name:        "override address is not on this machine",
			override:    "m1",
			wantErr:     []string{"m1", "10.0.0.1", "10.0.0.11", "--allow-address-mismatch"},
			wantWarning: nil,
		},
		{
			name:        "force accepts a non-local address for NAT",
			override:    "m1",
			force:       true,
			wantNode:    "w1",
			wantAddress: "10.0.0.1",
			wantWarning: []string{"NAT"},
		},
		{
			name:     "unknown node lists available names",
			override: "nope",
			wantErr:  []string{"nope", "m1", "w1"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := resolverFor(&FakeLister{Interfaces: local}, "w1", missingNodeConfig(t))
			res, err := r.Resolve(cfg, tc.override, tc.force)
			if len(tc.wantErr) > 0 {
				if err == nil {
					t.Fatalf("Resolve() = nil error, want error containing %v", tc.wantErr)
				}
				for _, want := range tc.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("Resolve() error = %q, want substring %q", err, want)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve() error = %v", err)
			}
			if res.NodeName() != tc.wantNode {
				t.Errorf("NodeName() = %q, want %q", res.NodeName(), tc.wantNode)
			}
			if res.Address != tc.wantAddress {
				t.Errorf("Address = %q, want %q", res.Address, tc.wantAddress)
			}
			for _, want := range tc.wantWarning {
				if !warningsContain(res.Warnings, want) {
					t.Errorf("Warnings = %q, want substring %q", res.Warnings, want)
				}
			}
			if len(tc.wantWarning) == 0 && len(res.Warnings) != 0 {
				t.Errorf("Warnings = %q, want none", res.Warnings)
			}
		})
	}
}

func TestResolveOverrideIgnoresNodeYAML(t *testing.T) {
	dir := t.TempDir()
	store := &Store{Dir: dir}
	if err := store.Save(&clusterconfig.NodeConfig{
		ClusterID: "prod",
		NodeName:  "w1",
		Address:   "10.0.0.11",
		Roles:     []clusterconfig.Role{clusterconfig.RoleWorker},
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	cfg := testCluster(t, "m1 10.0.0.1 master", "w1 10.0.0.11 worker")
	local := ifaces(t, "eth0 up 10.0.0.11/24")
	r := resolverFor(&FakeLister{Interfaces: local}, "w1", store.FilePath())

	// --node 优先于 node.yaml：即使身份文件写的是 w1，也要听 --node。
	res, err := r.Resolve(cfg, "m1", true)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if res.NodeName() != "w1" {
		t.Errorf("NodeName() = %q, want w1", res.NodeName())
	}
	if res.Host == nil || res.Host.Name != "m1" {
		t.Errorf("Host.Name = %+v, want m1", res.Host)
	}
	if res.NodeConfig == nil || res.NodeConfig.NodeName != "w1" {
		t.Errorf("NodeConfig = %+v, want the installed w1 identity", res.NodeConfig)
	}
	if !warningsContain(res.Warnings, "已安装") {
		t.Errorf("Warnings = %q, want a note about the installed identity", res.Warnings)
	}
}

// Resolve: node.yaml（优先级 2）

func TestResolveNodeConfigPrecedence(t *testing.T) {
	cfg := testCluster(t,
		"m1 10.0.0.1 master",
		"w1 10.0.0.11 worker",
	)

	t.Run("installed identity wins over interface matching", func(t *testing.T) {
		dir := t.TempDir()
		store := &Store{Dir: dir}
		if err := store.Save(&clusterconfig.NodeConfig{
			ClusterID: "prod",
			NodeName:  "w1",
			Address:   "10.0.0.11",
			Roles:     []clusterconfig.Role{clusterconfig.RoleWorker},
		}); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
		// 网卡同时有 m1 的地址，但身份文件说本机是 w1。
		local := ifaces(t, "eth0 up 10.0.0.1/24", "eth1 up 10.0.0.11/24")
		r := resolverFor(&FakeLister{Interfaces: local}, "w1", store.FilePath())

		res, err := r.Resolve(cfg, "", false)
		if err != nil {
			t.Fatalf("Resolve() error = %v", err)
		}
		if res.NodeName() != "w1" {
			t.Errorf("NodeName() = %q, want w1", res.NodeName())
		}
		if res.Address != "10.0.0.11" {
			t.Errorf("Address = %q, want 10.0.0.11", res.Address)
		}
	})

	t.Run("address change is a warning", func(t *testing.T) {
		dir := t.TempDir()
		store := &Store{Dir: dir}
		if err := store.Save(&clusterconfig.NodeConfig{
			ClusterID: "prod",
			NodeName:  "w1",
			Address:   "10.0.0.99", // 配置里已改成 10.0.0.11 / already changed to 10.0.0.11 in the config
			Roles:     []clusterconfig.Role{clusterconfig.RoleWorker},
		}); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
		r := resolverFor(&FakeLister{Interfaces: ifaces(t, "eth0 up 10.0.0.11/24")}, "w1", store.FilePath())
		res, err := r.Resolve(cfg, "", false)
		if err != nil {
			t.Fatalf("Resolve() error = %v", err)
		}
		if res.Address != "10.0.0.11" {
			t.Errorf("Address = %q, want the configured 10.0.0.11", res.Address)
		}
		if !warningsContain(res.Warnings, "已从 10.0.0.99 变为 10.0.0.11") {
			t.Errorf("Warnings = %q, want an address change note", res.Warnings)
		}
	})

	t.Run("node removed from config is an error", func(t *testing.T) {
		dir := t.TempDir()
		store := &Store{Dir: dir}
		if err := store.Save(&clusterconfig.NodeConfig{
			ClusterID: "prod",
			NodeName:  "gone",
			Address:   "10.0.0.200",
		}); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
		r := resolverFor(&FakeLister{Interfaces: ifaces(t, "eth0 up 10.0.0.11/24")}, "gone", store.FilePath())
		_, err := r.Resolve(cfg, "", false)
		if err == nil {
			t.Fatalf("Resolve() = nil error, want an error")
		}
		for _, want := range []string{"gone", "不在当前配置中"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("Resolve() error = %q, want substring %q", err, want)
			}
		}
	})

	t.Run("broken node.yaml is a warning and falls back", func(t *testing.T) {
		dir := t.TempDir()
		path := dir + "/node.yaml"
		if err := writeFile(path, "nodeName: \"\"\n"); err != nil {
			t.Fatalf("writeFile() error = %v", err)
		}
		r := resolverFor(&FakeLister{Interfaces: ifaces(t, "eth0 up 10.0.0.11/24")}, "w1", path)
		res, err := r.Resolve(cfg, "", false)
		if err != nil {
			t.Fatalf("Resolve() error = %v", err)
		}
		if !warningsContain(res.Warnings, "忽略该文件") {
			t.Errorf("Warnings = %q, want a note about the broken file", res.Warnings)
		}
	})
}

// Resolve: 网卡匹配（优先级 3）

func TestResolveByInterface(t *testing.T) {
	tests := []struct {
		name        string
		hosts       []string
		ifaces      []string
		hostname    string
		endpoints   []string
		wantNode    string
		wantAddress string
		wantErr     []string
		wantWarning []string
	}{
		{
			name:        "single exact match",
			hosts:       []string{"m1 10.0.0.1 master", "w1 10.0.0.11 worker"},
			ifaces:      []string{"eth0 up 10.0.0.11/24", "lo up loopback 127.0.0.1/8"},
			hostname:    "w1",
			wantNode:    "w1",
			wantAddress: "10.0.0.11",
		},
		{
			name:        "matchAddresses also matches (NAT)",
			hosts:       []string{"w1 172.16.0.9 worker 192.168.1.9"},
			ifaces:      []string{"eth0 up 192.168.1.9/24"},
			hostname:    "w1",
			wantNode:    "w1",
			wantAddress: "192.168.1.9",
		},
		{
			name:        "address and matchAddresses both local on one host",
			hosts:       []string{"w1 10.0.0.11 worker 192.168.1.9"},
			ifaces:      []string{"eth0 up 10.0.0.11/24", "eth1 up 192.168.1.9/24"},
			hostname:    "w1",
			wantNode:    "w1",
			wantAddress: "10.0.0.11",
			wantWarning: []string{"匹配到多个声明地址", "address 优先"},
		},
		{
			name:        "hostname differs from config identity",
			hosts:       []string{"w1 10.0.0.11 worker"},
			ifaces:      []string{"eth0 up 10.0.0.11/24"},
			hostname:    "node-42",
			wantNode:    "node-42",
			wantAddress: "10.0.0.11",
		},
		{
			name:     "zero match lists local and configured addresses",
			hosts:    []string{"m1 10.0.0.1 master", "w1 10.0.0.11 worker"},
			ifaces:   []string{"eth0 up 192.168.5.5/24"},
			hostname: "w9",
			wantErr:  []string{"192.168.5.5", "10.0.0.1", "10.0.0.11", "--node"},
		},
		{
			name:     "multi match reports both entries",
			hosts:    []string{"m1 10.0.0.1 master", "w1 10.0.0.11 worker"},
			ifaces:   []string{"eth0 up 10.0.0.1/24", "eth1 up 10.0.0.11/24"},
			hostname: "x",
			wantErr:  []string{"m1", "w1", "多个节点"},
		},
		{
			name:     "address declared by two hosts is a conflict",
			hosts:    []string{"m1 10.0.0.1 master", "w1 10.0.0.1 worker"},
			ifaces:   []string{"eth0 up 10.0.0.1/24"},
			hostname: "x",
			wantErr:  []string{"10.0.0.1", "m1", "w1"},
		},
		{
			name:        "VIP is excluded from matching",
			hosts:       []string{"m1 10.0.0.1 master", "m2 10.0.0.2 master", "w1 10.0.0.11 worker"},
			ifaces:      []string{"eth0 up 10.0.0.100/24", "eth1 up 10.0.0.11/24"},
			hostname:    "w1",
			endpoints:   []string{"10.0.0.100"},
			wantNode:    "w1",
			wantAddress: "10.0.0.11",
		},
		{
			name:        "externalEndpoints host is excluded",
			hosts:       []string{"m1 10.0.0.1 master", "w1 10.0.0.11 worker"},
			ifaces:      []string{"eth0 up 10.0.0.50/24", "eth1 up 10.0.0.11/24"},
			hostname:    "w1",
			endpoints:   []string{"https://10.0.0.50:6443"},
			wantNode:    "w1",
			wantAddress: "10.0.0.11",
		},
		{
			name:     "loopback and link-local addresses are excluded",
			hosts:    []string{"m1 127.0.0.1 master"},
			ifaces:   []string{"lo up loopback 127.0.0.1/8", "eth0 up 169.254.10.10/16", "eth1 up fe80::1/64"},
			hostname: "m1",
			wantErr:  []string{"127.0.0.1"},
		},
		{
			name:     "point-to-point interface is excluded",
			hosts:    []string{"w1 10.8.0.2 worker"},
			ifaces:   []string{"tun0 up ptp 10.8.0.2/32"},
			hostname: "w1",
			wantErr:  []string{"（无）"},
		},
		{
			name:     "down interface is ignored",
			hosts:    []string{"w1 10.0.0.11 worker"},
			ifaces:   []string{"eth0 10.0.0.11/24"},
			hostname: "w1",
			wantErr:  []string{"10.0.0.11"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testCluster(t, tc.hosts...)
			cfg.ControlPlane.ExternalEndpoints = tc.endpoints

			r := resolverFor(&FakeLister{Interfaces: ifaces(t, tc.ifaces...)}, tc.hostname, missingNodeConfig(t))
			res, err := r.Resolve(cfg, "", false)

			if len(tc.wantErr) > 0 {
				if err == nil {
					t.Fatalf("Resolve() = nil error, want error containing %v", tc.wantErr)
				}
				for _, want := range tc.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("Resolve() error = %q, want substring %q", err, want)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve() error = %v", err)
			}
			if res.NodeName() != tc.wantNode {
				t.Errorf("NodeName() = %q, want %q", res.NodeName(), tc.wantNode)
			}
			if res.Address != tc.wantAddress {
				t.Errorf("Address = %q, want %q", res.Address, tc.wantAddress)
			}
			for _, want := range tc.wantWarning {
				if !warningsContain(res.Warnings, want) {
					t.Errorf("Warnings = %q, want substring %q", res.Warnings, want)
				}
			}
			if len(tc.wantWarning) == 0 && len(res.Warnings) != 0 {
				t.Errorf("Warnings = %q, want none", res.Warnings)
			}
		})
	}
}

// TestExcludedInterfaceNames 逐一验证网卡名字排除表。
func TestExcludedInterfaceNames(t *testing.T) {
	excluded := []string{
		"docker0",
		"docker_gwbridge",
		"cni0",
		"kube-ipvs0",
		"kube-dummy-if",
		"cali1234abcd",
		"flannel.1",
		"cilium_host",
		"vxlan.calico",
		"tun0",
		"tap0",
		"br-1a2b3c",
		"veth9f0c",
		"virbr0",
		"lxcbr0",
		"tailscale0",
	}
	for _, name := range excluded {
		t.Run(name, func(t *testing.T) {
			if !isExcludedInterfaceName(name) {
				t.Fatalf("isExcludedInterfaceName(%q) = false, want true", name)
			}
			cfg := testCluster(t, "w1 10.0.0.11 worker")
			r := resolverFor(&FakeLister{Interfaces: ifaces(t, name+" up 10.0.0.11/24")}, "w1", missingNodeConfig(t))
			if _, err := r.Resolve(cfg, "", false); err == nil {
				t.Fatalf("Resolve() = nil error, want the %s address to be excluded", name)
			}
		})
	}

	// eth0/ens18/ta0 等真实网卡名不能被误伤。
	for _, name := range []string{"eth0", "ens18", "enp3s0", "bond0", "ta0", "team0"} {
		t.Run("kept/"+name, func(t *testing.T) {
			if isExcludedInterfaceName(name) {
				t.Fatalf("isExcludedInterfaceName(%q) = true, want false", name)
			}
			cfg := testCluster(t, "w1 10.0.0.11 worker")
			r := resolverFor(&FakeLister{Interfaces: ifaces(t, name+" up 10.0.0.11/24")}, "w1", missingNodeConfig(t))
			res, err := r.Resolve(cfg, "", false)
			if err != nil {
				t.Fatalf("Resolve() error = %v", err)
			}
			if res.NodeName() != "w1" {
				t.Errorf("NodeName() = %q, want w1", res.NodeName())
			}
		})
	}
}

// TestResolveDefaultDependencies 用仅用于文档的地址走一遍“不注入”的代码路径
// （DefaultInterfaceLister + os.Hostname），使结论不依赖运行测试的主机网卡。
func TestResolveDefaultDependencies(t *testing.T) {
	const docAddr = "203.0.113.7" // RFC 5737 TEST-NET-3（文档专用示例地址） / RFC 5737 TEST-NET-3
	cfg := testCluster(t, "m1 "+docAddr+" master")

	r := &Resolver{NodeConfigPath: missingNodeConfig(t)}
	// 不带 --allow-address-mismatch：这台机器几乎不可能真的拥有 203.0.113.7。
	if _, err := r.Resolve(cfg, "m1", false); err == nil {
		t.Skip("this host really has 203.0.113.7 configured; skipping the default-lister path")
	}

	res, err := r.Resolve(cfg, "m1", true)
	if err != nil {
		t.Fatalf("Resolve(force) error = %v", err)
	}
	if !warningsContain(res.Warnings, "NAT") {
		t.Errorf("Resolve(force) = %+v, want a NAT warning", res)
	}
}

func TestResolveListerError(t *testing.T) {
	cfg := testCluster(t, "m1 10.0.0.1 master")
	r := resolverFor(&FakeLister{Err: fmt.Errorf("no interfaces")}, "m1", missingNodeConfig(t))
	_, err := r.Resolve(cfg, "", false)
	if err == nil {
		t.Fatalf("Resolve() = nil error, want a wrapped lister error")
	}
	if !strings.Contains(err.Error(), "no interfaces") {
		t.Errorf("Resolve() error = %q, want the underlying error", err)
	}
}

func TestResolveNilConfig(t *testing.T) {
	r := resolverFor(&FakeLister{}, "m1", missingNodeConfig(t))
	if _, err := r.Resolve(nil, "", false); err == nil {
		t.Fatalf("Resolve(nil) = nil error, want an error")
	}
}

func TestDefaultInterfaceLister(t *testing.T) {
	list, err := DefaultInterfaceLister()
	if err != nil {
		t.Fatalf("DefaultInterfaceLister() error = %v", err)
	}
	for _, iface := range list {
		if !iface.IsUp() {
			t.Errorf("interface %s: want the up flag", iface.Name)
		}
		if iface.Name == "" {
			t.Errorf("interface with empty name: %+v", iface)
		}
		for _, p := range iface.Addresses {
			if !p.IsValid() {
				t.Errorf("interface %s: invalid prefix %v", iface.Name, p)
			}
		}
	}
}

// 地址辅助函数

func TestEndpointHost(t *testing.T) {
	tests := map[string]string{
		"https://10.0.0.50:6443":       "10.0.0.50",
		"http://vip.example.com:6443/": "vip.example.com",
		"10.0.0.50:6443":               "10.0.0.50",
		"10.0.0.50":                    "10.0.0.50",
		"vip.example.com":              "vip.example.com",
		"[fd00::1]:6443":               "fd00::1",
		"":                             "",
	}
	for in, want := range tests {
		t.Run(in, func(t *testing.T) {
			if got := clusterconfig.EndpointHost(in); got != want {
				t.Errorf("clusterconfig.EndpointHost(%q) = %q, want %q", in, got, want)
			}
		})
	}
}

func TestConstantsDefaults(t *testing.T) {
	// 防止默认路径常量被改动后与本包的假设不一致。
	if constants.NodeConfigFile != constants.KubemonoDir+"/"+NodeConfigFileName {
		t.Errorf("constants.NodeConfigFile = %q, want %q", constants.NodeConfigFile, constants.KubemonoDir+"/"+NodeConfigFileName)
	}
	store := &Store{}
	if store.FilePath() != constants.NodeConfigFile {
		t.Errorf("Store{}.FilePath() = %q, want %q", store.FilePath(), constants.NodeConfigFile)
	}
}

func TestResolveNodeNamePolicies(t *testing.T) {
	cfg := testCluster(t, "m1 10.0.0.1 master")
	local := ifaces(t, "eth0 up 10.0.0.1/24")
	r := resolverFor(&FakeLister{Interfaces: local}, "node-a", missingNodeConfig(t))

	cfg.Cluster.Hosts[0].NodeName = clusterconfig.NodeNameFromAddress
	res, err := r.Resolve(cfg, "", false)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.NodeName() != "10.0.0.1" || res.Host == nil || res.Host.Name != "m1" {
		t.Fatalf("address policy = node %q host %+v", res.NodeName(), res.Host)
	}

	cfg.Cluster.Hosts[0].NodeName = "m1.example.com"
	res, err = r.Resolve(cfg, "", false)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.NodeName() != "m1.example.com" || res.Host == nil || res.Host.Name != "m1" {
		t.Fatalf("explicit policy = node %q host %+v", res.NodeName(), res.Host)
	}
}
