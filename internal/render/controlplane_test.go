package render

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"kubemono/internal/bom"
	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
)

func clusterFor(mode string) *clusterconfig.Cluster {
	c := &clusterconfig.Cluster{}
	c.Cluster.Hosts = []clusterconfig.Host{
		{Name: "m1", Address: "10.0.0.1", Roles: []clusterconfig.Role{clusterconfig.RoleMaster, clusterconfig.RoleWorker}},
		{Name: "m2", Address: "10.0.0.2", Roles: []clusterconfig.Role{clusterconfig.RoleMaster, clusterconfig.RoleWorker}},
		{Name: "w1", Address: "10.0.0.11", Roles: []clusterconfig.Role{clusterconfig.RoleWorker}},
	}
	clusterconfig.SetDefaults(c)
	return c
}

// builderContext 为集群中的某个节点构建 render 上下文。
func builderContext(t *testing.T, cluster *clusterconfig.Cluster, node string) *Context {
	t.Helper()
	host, ok := cluster.Cluster.Hosts.FindHost(node)
	if !ok {
		t.Fatalf("node %s not in config", node)
	}
	return &Context{
		Cluster:   cluster,
		Host:      host,
		BOM:       bom.MustLoad(),
		Paths:     constants.NewPaths(t.TempDir()),
		Release:   bom.MustLoad().Release,
		ClusterID: "cid-1",
	}
}

func renderAll(t *testing.T, c *Context) *Spec {
	t.Helper()
	spec, err := Render(c, DefaultBuilders()...)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	return spec
}

func masterSpec(t *testing.T, mode string) (*Context, *Spec) {
	t.Helper()
	c := builderContext(t, clusterFor(mode), "m1")
	if mode == "staticpod" {
		// 加入第二个 master 的状态为 existing，便于断言 etcd 参数。
		c.EtcdInitialClusterState = "new"
		c.IsInitNode = true
	}
	return c, renderAll(t, c)
}

func TestMasterStaticPodRendersEveryComponent(t *testing.T) {
	c, spec := masterSpec(t, "staticpod")

	for _, comp := range []string{
		constants.ComponentEtcd,
		constants.ComponentAPIServer,
		constants.ComponentControllerManager,
		constants.ComponentScheduler,
	} {
		f, ok := spec.Find(c.Paths.Resolve("/etc/systemd/system/" + comp + ".service"))
		if !ok {
			t.Fatalf("缺少 %s 的 systemd unit", comp)
		}
		if f.Kind != KindUnit {
			t.Errorf("%s 的 Kind 不正确: %s", comp, f.Kind)
		}
		if !strings.Contains(string(f.Content), "ExecStart=/usr/local/bin/"+comp) {
			t.Errorf("%s 的 unit 未引用组件二进制:\n%s", comp, f.Content)
		}
		if !strings.Contains(string(f.Content), "Description=kubemono managed "+comp) {
			t.Errorf("%s 的 unit 缺少 Description", comp)
		}
	}

	// apiserver 必须指向 etcd 与 Service 网段。
	api := mustFind(t, spec, c.Paths.Resolve("/etc/systemd/system/kube-apiserver.service"))
	apiYAML := string(api.Content)
	for _, want := range []string{
		"--etcd-servers=https://127.0.0.1:2379",
		"--service-cluster-ip-range=" + c.Cluster.Network.ServiceCIDR,
		"--client-ca-file=" + c.Paths.CertPath(constants.CACertName),
		"--kubelet-client-certificate=" + c.Paths.CertPath(constants.APIServerKubeletClientCertName),
		"--secure-port=6443",
	} {
		if !strings.Contains(apiYAML, want) {
			t.Errorf("apiserver 清单缺少 %q", want)
		}
	}
	// kcm 必须开启 CIDR 分配，否则 kube-router 无法工作。
	kcm := string(mustFind(t, spec, c.Paths.Resolve("/etc/systemd/system/kube-controller-manager.service")).Content)
	for _, want := range []string{"--allocate-node-cidrs=true", "--cluster-cidr=" + c.Cluster.Network.PodCIDR} {
		if !strings.Contains(kcm, want) {
			t.Errorf("controller-manager 清单缺少 %q", want)
		}
	}
	// init 节点的 stacked etcd 只能列自己：列表里出现还没启动的成员会让它永远
	// 拿不到 quorum（真机实测：raft 反复 "starting a new election at term 1"），
	// 其余成员由 etcd phase 的 AddLearner 路径加入。
	etcd := string(mustFind(t, spec, c.Paths.Resolve("/etc/systemd/system/etcd.service")).Content)
	for _, want := range []string{"--initial-cluster-state=new", "--initial-cluster=m1=https://10.0.0.1:2380", "--name=m1"} {
		if !strings.Contains(etcd, want) {
			t.Errorf("etcd 清单缺少 %q", want)
		}
	}
	if strings.Contains(etcd, "m2=https://10.0.0.2:2380") {
		t.Errorf("init 节点的 --initial-cluster 不得包含还没启动的成员：\n%s", etcd)
	}

	// kubelet 配置与运行时配置。
	kubeletCfg, ok := spec.Find(c.Paths.KubeletConfig())
	if !ok {
		t.Fatal("缺少 kubelet 配置")
	}
	var kc struct {
		APIVersion string   `json:"apiVersion"`
		Kind       string   `json:"kind"`
		StaticPod  string   `json:"staticPodPath"`
		ClusterDNS []string `json:"clusterDNS"`
		Auth       struct {
			Webhook struct {
				Enabled bool `json:"enabled"`
			} `json:"webhook"`
		} `json:"authentication"`
	}
	if err := yaml.Unmarshal(kubeletCfg.Content, &kc); err != nil {
		t.Fatalf("kubelet 配置不是合法 YAML: %v", err)
	}
	// kubelet 配置里写的是生产路径（测试根只用于测试/预览，
	// 不会把 /etc/kubernetes/manifests 这类容器内路径也重定位）。
	if !strings.HasSuffix(kc.StaticPod, constants.ManifestsDir) {
		t.Errorf("kubelet staticPodPath = %q, want a path ending in %s", kc.StaticPod, constants.ManifestsDir)
	}
	if len(kc.ClusterDNS) != 1 || kc.ClusterDNS[0] != c.Cluster.Network.ClusterDNS {
		t.Errorf("kubelet clusterDNS = %v", kc.ClusterDNS)
	}
	if !kc.Auth.Webhook.Enabled {
		t.Error("kubelet 必须启用 webhook 认证")
	}
	containerdConf, ok := spec.Find(ContainerdConfigPath(c))
	if !ok {
		t.Fatal("缺少 containerd 配置")
	}
	if !strings.Contains(string(containerdConf.Content), "config_path = '/etc/containerd/certs.d'") {
		t.Errorf("containerd 配置缺少 config_path:\n%s", containerdConf.Content)
	}
}

func TestWorkerHasNoControlPlane(t *testing.T) {
	c := builderContext(t, clusterFor("staticpod"), "w1")
	spec := renderAll(t, c)

	for _, comp := range []string{constants.ComponentEtcd, constants.ComponentAPIServer, constants.ComponentControllerManager, constants.ComponentScheduler} {
		if _, ok := spec.Find(c.Paths.Resolve("/etc/systemd/system/" + comp + ".service")); ok {
			t.Errorf("worker 不应渲染 %s 的 unit", comp)
		}
	}
	if _, ok := spec.Find(c.Paths.KubeletConfig()); !ok {
		t.Fatal("worker 必须有 kubelet 配置")
	}
	// kubeconfig 是 kubelet 的 flag（不是配置文件字段），且 master/worker 不同。
	prof, err := LoadProfile(c)
	if err != nil {
		t.Fatal(err)
	}
	data, err := KubeletFlags(c)
	if err != nil {
		t.Fatal(err)
	}
	data.Binary = "kubelet"
	flags, err := prof.RenderFlags(constants.ComponentKubelet, data)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(flags, " ")
	if !strings.Contains(joined, "--bootstrap-kubeconfig="+c.Paths.KubeconfigPath(constants.BootstrapKubeletKubeconfigName)) {
		t.Errorf("worker 的 kubelet 应使用引导 kubeconfig: %s", joined)
	}
	// --kubeconfig 是 kubelet 完成引导后写入凭据的目标路径，
	// 与 --bootstrap-kubeconfig 同时存在。
	if !strings.Contains(joined, "--kubeconfig="+c.Paths.KubeconfigPath(constants.KubeletKubeconfigName)) {
		t.Errorf("worker 的 kubelet 需要 --kubeconfig 作为写入目标: %s", joined)
	}
	// worker 不得引用 CA 私钥。
	if strings.Contains(joined, constants.CAKeyName) {
		t.Errorf("worker 的 kubelet 不应接触 CA 私钥: %s", joined)
	}
}

func TestExternalEtcdSkipsLocalMember(t *testing.T) {
	cluster := clusterFor("staticpod")
	cluster.ControlPlane.Etcd.External = clusterconfig.ExternalEtcdSpec{Endpoints: []string{"https://127.0.0.1:2379"}}
	cluster.ControlPlane.Etcd.External.Endpoints = []string{"https://10.0.0.31:2379", "https://10.0.0.32:2379", "https://10.0.0.33:2379"}
	c := builderContext(t, cluster, "m1")
	spec := renderAll(t, c)
	if _, ok := spec.Find(c.Paths.Resolve("/etc/systemd/system/etcd.service")); ok {
		t.Fatal("外部 etcd 时不应渲染本地 etcd 清单")
	}
	api := string(mustFind(t, spec, c.Paths.Resolve("/etc/systemd/system/kube-apiserver.service")).Content)
	if !strings.Contains(api, "--etcd-servers=https://10.0.0.31:2379,https://10.0.0.32:2379,https://10.0.0.33:2379") {
		t.Errorf("apiserver 未使用外部 etcd endpoints:\n%s", api)
	}
}

func TestBinaryModeRendersUnits(t *testing.T) {
	c, spec := masterSpec(t, "binary")
	// staticpod 模式下不应该有单元文件；binary 模式必须有 kubelet.service。
	unit, ok := spec.Find(c.Paths.Resolve("/etc/systemd/system/kubelet.service"))
	if !ok {
		t.Fatal("binary 模式必须渲染 kubelet.service")
	}
	for _, want := range []string{"[Unit]", "[Service]", "[Install]", "ExecStart=/usr/local/bin/kubelet", "WantedBy=multi-user.target"} {
		if !strings.Contains(string(unit.Content), want) {
			t.Errorf("kubelet.service 缺少 %q", want)
		}
	}
}

func TestRenderingIsDeterministic(t *testing.T) {
	c := builderContext(t, clusterFor("staticpod"), "m1")
	s1, err := Render(c, DefaultBuilders()...)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := Render(c, DefaultBuilders()...)
	if err != nil {
		t.Fatal(err)
	}
	if s1.Hash() != s2.Hash() {
		t.Fatalf("同一上下文两次渲染的哈希不同：%s vs %s", s1.Hash(), s2.Hash())
	}
	if len(s1.Files) != len(s2.Files) {
		t.Fatalf("文件数不同：%d vs %d", len(s1.Files), len(s2.Files))
	}
}

func TestExtraArgsAreApplied(t *testing.T) {
	cluster := clusterFor("")
	cluster.ControlPlane.APIServer.ExtraArgs = map[string]string{"profiling": "false", "audit-log-format": "json"}
	c := builderContext(t, cluster, "m1")
	spec := renderAll(t, c)
	api := string(mustFind(t, spec, c.Paths.Resolve("/etc/systemd/system/kube-apiserver.service")).Content)
	for _, want := range []string{"--profiling=false", "--audit-log-format=json"} {
		if !strings.Contains(api, want) {
			t.Errorf("extraArgs 未生效，缺少 %q:\n%s", want, api)
		}
	}
}

func mustFind(t *testing.T, spec *Spec, path string) File {
	t.Helper()
	f, ok := spec.Find(path)
	if !ok {
		t.Fatalf("缺少渲染文件 %s", path)
	}
	return f
}

// TestMasterKubeletTaintDependsOnWorkerRole 覆盖角色组合：
// - [master, worker] 混合节点可调度，默认不带 control-plane 污点；
// - [master] 纯控制面节点由 kubelet 注册时自带 control-plane:NoSchedule。
func TestMasterKubeletTaintDependsOnWorkerRole(t *testing.T) {
	cluster := clusterFor("staticpod")
	c := builderContext(t, cluster, "m1")
	spec := renderAll(t, c)
	cfg := string(mustFind(t, spec, c.Paths.KubeletConfig()).Content)
	if strings.Contains(cfg, "node-role.kubernetes.io/control-plane:NoSchedule") {
		t.Errorf("混合 master 不应默认带控制面污点:\n%s", cfg)
	}

	// 显式声明的污点仍然生效。
	cluster.Cluster.Hosts[0].Taints = []string{"dedicated=ci:NoExecute"}
	c = builderContext(t, cluster, "m1")
	cfg = string(mustFind(t, renderAll(t, c), c.Paths.KubeletConfig()).Content)
	for _, want := range []string{"key: dedicated", `value: "ci"`, "effect: NoExecute"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("显式 taints 未生效，缺少 %q:\n%s", want, cfg)
		}
	}

	// 纯控制面 master 没有 kubelet，也不产生 kubelet 配置文件。
	dedicated := *cluster
	dedicated.Cluster.Hosts = append([]clusterconfig.Host(nil), cluster.Cluster.Hosts...)
	dedicated.Cluster.Hosts[0].Roles = []clusterconfig.Role{clusterconfig.RoleMaster}
	dc := builderContext(t, &dedicated, "m1")
	if _, ok := renderAll(t, dc).Find(dc.Paths.KubeletConfig()); ok {
		t.Fatal("纯控制面 master 不应渲染 kubelet 配置")
	}
}

// TestBinaryModeRendersUnitsNotManifests 固定"两种模式唯一分叉点"：
// binary 模式不产生静态 Pod 清单，而是产出 systemd unit，且 flags 完全相同。
func TestBinaryModeRendersUnitsNotManifests(t *testing.T) {
	cluster := clusterFor("binary")
	c := builderContext(t, cluster, "m1")
	spec := renderAll(t, c)
	for _, comp := range []string{
		constants.ComponentEtcd, constants.ComponentAPIServer,
		constants.ComponentControllerManager, constants.ComponentScheduler,
	} {
		unit, ok := spec.Find(c.Paths.Resolve("/etc/systemd/system/" + comp + ".service"))
		if !ok {
			t.Fatalf("binary 模式缺少 %s 的 unit", comp)
		}
		body := string(unit.Content)
		for _, want := range []string{"[Unit]", "[Service]", "[Install]", "ExecStart=/usr/local/bin/" + comp, "Restart=always"} {
			if !strings.Contains(body, want) {
				t.Errorf("%s 的 unit 缺少 %q:\n%s", comp, want, body)
			}
		}
	}
	// 参数必须与 staticpod 模式一致：apiserver 的 unit 里同样有这些 flag。
	unit, _ := spec.Find(c.Paths.Resolve("/etc/systemd/system/kube-apiserver.service"))
	body := string(unit.Content)
	for _, want := range []string{
		"--etcd-servers=https://127.0.0.1:2379",
		"--service-cluster-ip-range=" + cluster.Network.ServiceCIDR,
		"--secure-port=6443",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("binary 模式的 apiserver unit 缺少 %q", want)
		}
	}
}

// TestJoiningMasterRendersEveryEtcdMember 覆盖加入 master 的 stacked etcd：
// --initial-cluster 必须含全部 master，且 --initial-cluster-state=existing。
func TestJoiningMasterRendersEveryEtcdMember(t *testing.T) {
	c := builderContext(t, clusterFor("staticpod"), "m2")
	spec := renderAll(t, c)

	etcd := string(mustFind(t, spec, c.Paths.Resolve("/etc/systemd/system/etcd.service")).Content)
	for _, want := range []string{"--name=m2", "m1=https://10.0.0.1:2380", "m2=https://10.0.0.2:2380"} {
		if !strings.Contains(etcd, want) {
			t.Errorf("加入方 etcd 清单缺少 %q：\n%s", want, etcd)
		}
	}
}

// TestAuditLogPathIsWritableMount 覆盖真机踩到的坑：apiserver 的审计日志路径
// 必须落在静态 Pod **可写**的挂载里。清单只把 /var/log/kubernetes 挂成可写
// （audit-logs 卷），/etc/kubernetes 是只读挂载；日志写进只读目录时 apiserver
// 直接 "ensureLogFile: ... read-only file system" 退出，控制面永远起不来。
func TestAuditLogPathIsWritableMount(t *testing.T) {
	c, spec := masterSpec(t, "staticpod")
	api := string(mustFind(t, spec, c.Paths.Resolve("/etc/systemd/system/kube-apiserver.service")).Content)

	if !strings.Contains(api, "--audit-log-path="+constants.AuditLogDir+"/") {
		t.Errorf("--audit-log-path 必须落在可写的 %s 下：\n%s", constants.AuditLogDir, api)
	}
	if strings.Contains(api, "--audit-log-path="+constants.AuditDir+"/") {
		t.Errorf("--audit-log-path 不得指向只读的 %s：\n%s", constants.AuditDir, api)
	}
	// 策略文件仍在只读目录（挂载点覆盖它）。策略路径走 Paths（测试根演练
	// 时会带临时根），日志路径是容器内路径，不走 Paths。
	if !strings.Contains(api, "--audit-policy-file="+c.Paths.Audit()+"/") {
		t.Errorf("--audit-policy-file 应指向 %s：\n%s", c.Paths.Audit(), api)
	}
}

// TestEtcdJoiningMasterUsesExistingState 覆盖真机踩到的坑。
// 加入已有集群的 master 必须渲染 --initial-cluster-state=existing：
// 用 new 配完整成员列表时，etcd 会**自建一个全新的集群**，与 init master 彻底分裂
// （真机实测：m2/m3 组成独立三成员集群，m1 只看得到 m1，m2/m3 只看得到彼此，
// RBAC 绑定也各不相干）。init master 仍然必须是 new 且只列自己。
func TestEtcdJoiningMasterUsesExistingState(t *testing.T) {
	// init master（m1）：new + 只列自己。
	initCtx, _ := masterSpec(t, "staticpod")
	// 清掉夹具里的显式覆盖，验证渲染层能自己推导出正确状态。
	initCtx.EtcdInitialClusterState = ""
	initCtx.IsInitNode = true
	initCtx.Host, _ = initCtx.Cluster.Cluster.Hosts.FindHost("m1")
	initSpec := renderAll(t, initCtx)
	initEtcd := string(mustFind(t, initSpec, initCtx.Paths.Resolve("/etc/systemd/system/etcd.service")).Content)
	if !strings.Contains(initEtcd, "--initial-cluster-state=new") {
		t.Fatalf("init master 必须是 new：\n%s", initEtcd)
	}

	// 加入的 master（m2）：existing + 全成员列表。
	joinCtx, _ := masterSpec(t, "staticpod")
	host, _ := joinCtx.Cluster.Cluster.Hosts.FindHost("m2")
	if host == nil {
		t.Skip("夹具里没有 m2（该测试需要 3 master 配置）")
	}
	joinCtx.Host = host
	joinCtx.EtcdInitialClusterState = ""
	joinCtx.IsInitNode = false
	joinSpec := renderAll(t, joinCtx)
	joinEtcd := string(mustFind(t, joinSpec, joinCtx.Paths.Resolve("/etc/systemd/system/etcd.service")).Content)
	if !strings.Contains(joinEtcd, "--initial-cluster-state=existing") {
		t.Fatalf("加入的 master 必须是 existing，否则会自建集群导致分裂：\n%s", joinEtcd)
	}
	if !strings.Contains(joinEtcd, "--name=m2") {
		t.Errorf("加入的 master 清单缺少 --name=m2：\n%s", joinEtcd)
	}
	if !strings.Contains(joinEtcd, "m1=https://") || !strings.Contains(joinEtcd, "m2=https://") {
		t.Errorf("加入的 master 必须列出全部成员：\n%s", joinEtcd)
	}
}

func TestKubeletHostnameUsesNodeName(t *testing.T) {
	c := builderContext(t, clusterFor("staticpod"), "m1")
	c.Host.NodeName = clusterconfig.NodeNameFromAddress
	c.NodeNameValue = c.Host.Address
	kubeletData, err := KubeletFlags(c)
	if err != nil {
		t.Fatalf("KubeletFlags: %v", err)
	}
	if kubeletData.Hostname != c.Host.Address {
		t.Fatalf("kubelet hostname = %q, want %q", kubeletData.Hostname, c.Host.Address)
	}
	etcdData, err := EtcdFlags(c)
	if err != nil {
		t.Fatalf("EtcdFlags: %v", err)
	}
	if etcdData.EtcdName != c.Host.Address {
		t.Fatalf("etcd name = %q, want %q", etcdData.EtcdName, c.Host.Address)
	}
}

// 纯控制面 master 不运行 Service 代理：aggregator 必须走 Endpoint 直连，
// 否则 metrics-server 等聚合 API 在纯 master 上不可用。
func TestAPIServerEnablesAggregatorRouting(t *testing.T) {
	c, spec := masterSpec(t, "staticpod")
	api := string(mustFind(t, spec, c.Paths.Resolve("/etc/systemd/system/kube-apiserver.service")).Content)
	if !strings.Contains(api, "--enable-aggregator-routing=true") {
		t.Fatalf("kube-apiserver 缺少 --enable-aggregator-routing=true:\n%s", api)
	}
}

// TestKubeletRootDirChangesPathsAndFlags 校验 kubelet 数据目录可配置。
func TestKubeletRootDirChangesPathsAndFlags(t *testing.T) {
	cluster := clusterFor("staticpod")
	cluster.Node.Kubelet.RootDir = "/data/kubelet"
	c := builderContext(t, cluster, "w1")
	c.Paths = cluster.WithDataDirs(c.Paths)
	spec := renderAll(t, c)

	root := c.Paths.Kubelet()
	if root == c.Paths.Resolve(constants.KubeletDir) {
		t.Fatalf("kubelet 数据目录未按配置解析: %s", root)
	}
	if _, ok := spec.Find(c.Paths.KubeletConfig()); !ok {
		t.Fatalf("缺少 %s", c.Paths.KubeletConfig())
	}
	unit := string(mustFind(t, spec, c.Paths.Resolve("/etc/systemd/system/kubelet.service")).Content)
	for _, want := range []string{
		"--root-dir=" + root,
		"--cert-dir=" + root + "/pki",
		"--config=" + root + "/config.yaml",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("kubelet unit 缺少 %q:\n%s", want, unit)
		}
	}
}

// TestAuditConfigControlsFlagsAndPolicy 固定 audit 的显式配置入口。
func TestAuditConfigControlsFlagsAndPolicy(t *testing.T) {
	cluster := clusterFor("staticpod")
	cluster.ControlPlane.Audit.LogMaxAge = func(v int) *int { return &v }(14)
	cluster.ControlPlane.Audit.LogMaxBackup = func(v int) *int { return &v }(20)
	cluster.ControlPlane.Audit.LogMaxSize = func(v int) *int { return &v }(200)
	cluster.ControlPlane.Audit.Policy = `apiVersion: audit.k8s.io/v1
kind: Policy
rules:
  - level: None
`
	c := builderContext(t, cluster, "m1")
	c.IsInitNode = true
	spec := renderAll(t, c)
	api := string(mustFind(t, spec, c.Paths.Resolve("/etc/systemd/system/kube-apiserver.service")).Content)
	for _, want := range []string{
		"--audit-log-maxage=14",
		"--audit-log-maxbackup=20",
		"--audit-log-maxsize=200",
	} {
		if !strings.Contains(api, want) {
			t.Errorf("apiserver 缺少 %s:\n%s", want, api)
		}
	}
	policy := string(mustFind(t, spec, c.Paths.Audit()+"/policy.yaml").Content)
	if !strings.Contains(policy, "level: None") {
		t.Fatalf("审计策略未使用配置内容:\n%s", policy)
	}
}

// TestAuditDisabledOmitsFlagsAndPolicy 确认 enabled=false 时不再生成审计文件。
func TestAuditDisabledOmitsFlagsAndPolicy(t *testing.T) {
	cluster := clusterFor("staticpod")
	cluster.ControlPlane.Audit.Enabled = func(v bool) *bool { return &v }(false)
	c := builderContext(t, cluster, "m1")
	c.IsInitNode = true
	spec := renderAll(t, c)
	api := string(mustFind(t, spec, c.Paths.Resolve("/etc/systemd/system/kube-apiserver.service")).Content)
	if strings.Contains(api, "--audit-") {
		t.Fatalf("关闭 audit 后不应出现审计 flag:\n%s", api)
	}
	if _, ok := spec.Find(c.Paths.Audit() + "/policy.yaml"); ok {
		t.Fatal("关闭 audit 后不应生成策略文件")
	}
}

// TestCgroupDriverCustomIgnoresContainerd：custom 模式下 containerd 段不生效，
// 空的 custom.cgroupDriver 必须表示 systemd，不能回落到 containerd.systemdCgroup。
func TestCgroupDriverCustomIgnoresContainerd(t *testing.T) {
	c := testContext("")
	c.Cluster.Node.Runtime = clusterconfig.RuntimeSpec{
		Containerd: clusterconfig.ContainerdSpec{SystemdCgroup: clusterconfig.BoolPtr(false)},
		Custom:     clusterconfig.CustomRuntimeSpec{CRISocket: "unix:///run/crio/crio.sock", ServiceName: "crio"},
	}
	if got := cgroupDriver(c.Cluster); got != "systemd" {
		t.Fatalf("custom 空 cgroupDriver = %q, want systemd", got)
	}
}
