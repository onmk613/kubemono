package render

import (
	"fmt"
	"kubemono/internal/util/stringsx"
	"sort"
	"strings"

	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	"kubemono/internal/profile"
)

// ControlPlaneComponent 是以 systemd unit 渲染的控制面组件。
//
// 这里只有组件名：健康端点（scheme/port/path）以
// internal/controlplane.defaultHealth 为唯一来源，这里不重复登记
// HealthPath/HealthPort/HealthScheme。
type ControlPlaneComponent struct {
	// Name 是 BOM/profile 里的组件名。
	Name string
}

// ControlPlaneComponents 按启动顺序返回控制面的组件。
func ControlPlaneComponents() []ControlPlaneComponent {
	return []ControlPlaneComponent{
		{Name: constants.ComponentAPIServer},
		{Name: constants.ComponentControllerManager},
		{Name: constants.ComponentScheduler},
	}
}

// RunsEtcd 判断 etcd 是否在本机作为本地成员运行。
func RunsEtcd(c *Context) bool {
	if c.Cluster == nil || c.Host == nil {
		return false
	}
	return !c.Cluster.ControlPlane.Etcd.IsExternal() && c.Host.IsMaster()
}

// CommonData 返回所有组件共享的公共字段（BOM/身份守卫在组件构造器里统一做）。
func CommonData(c *Context) (profile.CommonFlags, error) {
	if c.Cluster == nil || c.Host == nil {
		return profile.CommonFlags{}, fmt.Errorf("render: 缺少集群配置或节点身份")
	}
	if c.BOM == nil {
		return profile.CommonFlags{}, fmt.Errorf("render: 缺少 BOM")
	}
	return profile.CommonFlags{Hostname: c.NodeName()}, nil
}

// APIServerFlags 构造 kube-apiserver 的模板数据。
// 证书/审计/加密配置全部走 constants.Paths 的约定路径，只有 external etcd
// 的凭据来自用户配置。
func APIServerFlags(c *Context) (*profile.APIServerFlags, error) {
	common, err := CommonData(c)
	if err != nil {
		return nil, err
	}
	spec := c.Cluster
	paths := c.Paths
	host := c.Host

	data := &profile.APIServerFlags{CommonFlags: common}
	data.AdvertiseAddress = host.Address
	data.APIServerPort = constants.APIServerPort
	data.ServiceCIDR = spec.Network.ServiceCIDR
	data.ClusterDomain = spec.Network.DNSDomain
	data.APIServerCertFile = paths.CertPath(constants.APIServerCertName)
	data.APIServerKeyFile = paths.CertPath(constants.APIServerKeyName)
	data.ClientCAFile = paths.CertPath(constants.CACertName)
	data.KubeletClientCertFile = paths.CertPath(constants.APIServerKubeletClientCertName)
	data.KubeletClientKeyFile = paths.CertPath(constants.APIServerKubeletClientKeyName)
	data.FrontProxyCACertFile = paths.CertPath(constants.FrontProxyCACertName)
	data.FrontProxyClientCertFile = paths.CertPath(constants.FrontProxyClientCertName)
	data.FrontProxyClientKeyFile = paths.CertPath(constants.FrontProxyClientKeyName)
	data.ServiceAccountPubFile = paths.ServiceAccountPub()
	data.ServiceAccountPrivateKeyFile = paths.ServiceAccountKey()
	data.EtcdServers = etcdServers(c)
	if spec.ControlPlane.Etcd.IsExternal() {
		// external etcd 由用户管理：apiserver 直接使用用户提供的 CA/客户端证书。
		data.EtcdCAFile = spec.ControlPlane.Etcd.External.CAFile
		data.EtcdCertFile = spec.ControlPlane.Etcd.External.CertFile
		data.EtcdKeyFile = spec.ControlPlane.Etcd.External.KeyFile
	} else {
		data.EtcdCAFile = paths.EtcdCertPath(constants.CACertName)
		data.EtcdCertFile = paths.CertPath(constants.APIServerEtcdClientCertName)
		data.EtcdKeyFile = paths.CertPath(constants.APIServerEtcdClientKeyName)
	}
	audit := spec.ControlPlane.Audit
	data.AuditEnabled = audit.IsEnabled()
	// 策略放 /etc/kubernetes/audit，日志写 /var/log/kubernetes；两者都在本机，
	// 由 apiserver 进程按文件权限访问。
	data.AuditPolicyFile = paths.Audit() + "/" + constants.AuditPolicyName
	data.AuditLogFile = constants.AuditLogDir + "/audit.log"
	data.AuditMaxAge = audit.MaxAge()
	data.AuditMaxBackup = audit.MaxBackup()
	data.AuditMaxSize = audit.MaxSize()
	data.AuditPolicy = audit.Policy
	if strings.TrimSpace(data.AuditPolicy) == "" {
		data.AuditPolicy = auditPolicyYAML
	}
	// Secret 静态加密是数据格式/密钥生命周期的一部分：关闭或轮换会让已有
	// 加密 Secret 不可读，因此不提供在线开关；详见 docs/cluster-config.md。
	data.EncryptionEnabled = true
	data.EncryptionConfigFile = paths.EncryptionConfig() + "/" + constants.EncryptionConfigName
	return data, nil
}

// ControllerManagerFlags 构造 kube-controller-manager 的模板数据。
func ControllerManagerFlags(c *Context) (*profile.ControllerManagerFlags, error) {
	common, err := CommonData(c)
	if err != nil {
		return nil, err
	}
	spec := c.Cluster
	paths := c.Paths

	data := &profile.ControllerManagerFlags{CommonFlags: common}
	data.ControllerManagerKubeconfig = paths.KubeconfigPath(constants.ControllerManagerKubeconfigName)
	data.ClusterCIDR = spec.Network.PodCIDR
	if spec.Network.NodeCIDRMaskSize != nil {
		data.NodeCIDRMaskSize = *spec.Network.NodeCIDRMaskSize
	}
	data.ClusterSigningCertFile = paths.CertPath(constants.CACertName)
	data.ClusterSigningKeyFile = paths.CertPath(constants.CAKeyName)
	data.RootCAFile = paths.CertPath(constants.CACertName)
	data.ClientCAFile = paths.CertPath(constants.CACertName)
	data.FrontProxyCACertFile = paths.CertPath(constants.FrontProxyCACertName)
	data.ServiceAccountPrivateKeyFile = paths.ServiceAccountKey()
	return data, nil
}

// SchedulerFlags 构造 kube-scheduler 的模板数据（只有 kubeconfig 一个参数）。
func SchedulerFlags(c *Context) (*profile.SchedulerFlags, error) {
	common, err := CommonData(c)
	if err != nil {
		return nil, err
	}
	return &profile.SchedulerFlags{
		CommonFlags:         common,
		SchedulerKubeconfig: c.Paths.KubeconfigPath(constants.SchedulerKubeconfigName),
	}, nil
}

// EtcdFlags 构造本机 etcd 成员（stacked 形态）的模板数据。
func EtcdFlags(c *Context) (*profile.EtcdFlags, error) {
	common, err := CommonData(c)
	if err != nil {
		return nil, err
	}
	spec := c.Cluster
	paths := c.Paths
	host := c.Host
	name := c.NodeName()

	data := &profile.EtcdFlags{CommonFlags: common}
	data.EtcdName = name
	data.EtcdDataDir = spec.ControlPlane.Etcd.Local.DataDir
	data.EtcdServerTrustedCAFile = paths.EtcdCertPath(constants.CACertName)
	data.EtcdServerCertFile = paths.EtcdCertPath("server.crt")
	data.EtcdServerKeyFile = paths.EtcdCertPath("server.key")
	data.EtcdPeerTrustedCAFile = paths.EtcdCertPath(constants.CACertName)
	data.EtcdPeerCertFile = paths.EtcdCertPath("peer.crt")
	data.EtcdPeerKeyFile = paths.EtcdCertPath("peer.key")
	data.EtcdListenClientURLs = []string{"https://127.0.0.1:2379", "https://" + host.Address + ":2379"}
	data.EtcdAdvertiseClientURLs = []string{"https://" + host.Address + ":2379"}
	data.EtcdListenPeerURLs = []string{"https://" + host.Address + ":2380"}
	data.EtcdInitialAdvertisePeerURLs = []string{"https://" + host.Address + ":2380"}
	// 多 master 的 stacked etcd：--initial-cluster 必须列出全部成员，
	// 加入已有集群的 master 用 existing（否则 etcd 会试图自建集群）。
	data.EtcdInitialCluster = c.EtcdInitialCluster
	if len(data.EtcdInitialCluster) == 0 {
		data.EtcdInitialCluster = etcdInitialClusterList(c)
	}
	if len(data.EtcdInitialCluster) == 0 {
		data.EtcdInitialCluster = []string{name + "=https://" + host.Address + ":2380"}
	}
	data.EtcdInitialClusterState = c.EtcdInitialClusterState
	if data.EtcdInitialClusterState == "" {
		// 加入已有集群的 master 必须是 existing。用 new + 全成员列表时，
		// etcd 会**自建一个全新的集群**：真机实测 m2/m3 组成独立的三成员集群，
		// 与 m1 彻底分裂（m1 只看得到 m1，m2/m3 只看得到彼此，RBAC 绑定也各不相干）。
		// 只有显式 init 的 master 自建 etcd，与 --initial-cluster 只列自己保持一致。
		if c.IsInitNode {
			data.EtcdInitialClusterState = "new"
		} else {
			data.EtcdInitialClusterState = "existing"
		}
	}
	data.EtcdMetricsURLs = fmt.Sprintf("http://127.0.0.1:%d", constants.EtcdMetricsPort)
	data.EtcdSnapshotCount = 10000
	data.EtcdQuotaBackendBytes = 2147483648 // 2GiB
	data.EtcdAutoCompactionRetention = "8"
	return data, nil
}

// KubeletFlags 构造 kubelet（节点代理）的模板数据。
// flags 与 kubelet-config.yaml 两个模板共用同一份数据。
func KubeletFlags(c *Context) (*profile.KubeletFlags, error) {
	common, err := CommonData(c)
	if err != nil {
		return nil, err
	}
	spec := c.Cluster
	paths := c.Paths
	host := c.Host

	data := &profile.KubeletFlags{CommonFlags: common}
	data.KubeletConfigFile = paths.KubeletConfig()
	data.KubeletCertDir = paths.Kubelet() + "/pki"
	data.KubeletRootDir = paths.Kubelet()
	data.KubeletResolvConf = "/etc/resolv.conf"
	data.KubeletCgroupDriver = cgroupDriver(spec)
	data.NodeIP = host.Address
	data.ContainerRuntimeEndpoint = runtimeEndpoint(spec)
	data.ClientCAFile = paths.CertPath(constants.CACertName)
	data.ClusterDomain = spec.Network.DNSDomain
	if host.IsMaster() {
		// master 的 kubelet 直接用本机直签的凭据。
		data.KubeletKubeconfigFile = paths.KubeconfigPath(constants.KubeletKubeconfigName)
	} else {
		data.BootstrapKubeconfigFile = paths.KubeconfigPath(constants.BootstrapKubeletKubeconfigName)
		data.KubeletKubeconfigFile = paths.KubeconfigPath(constants.KubeletKubeconfigName)
	}
	if dns := spec.Network.ClusterDNS; dns != "" {
		data.KubeletClusterDNS = []string{dns}
	}
	// 污点：控制面节点的标准污点 + 配置里为该节点声明的污点。
	if host.IsMaster() && !host.IsWorker() {
		// 纯控制面 master 默认不可调度；[master, worker] 混合节点要跑业务，
		// 默认不加控制面污点（需要时可在 hosts[].taints 显式声明）。
		data.RegisterWithTaints = append(data.RegisterWithTaints,
			"node-role.kubernetes.io/control-plane:NoSchedule")
	}
	data.RegisterWithTaints = append(data.RegisterWithTaints, host.Taints...)
	data.RegisterWithTaints = stringsx.DedupSorted(data.RegisterWithTaints)
	return data, nil
}

// flagsCommoner 让 render 分支统一设置组件数据的公共字段。
type flagsCommoner interface {
	FlagsCommon() *profile.CommonFlags
}

// ControlPlaneFlags 返回控制面组件的模板数据（apiserver/controller-manager/
// scheduler/etcd）。
// ExtraArgs 由各组件渲染分支自行设置——clusterconfig.ExtraArgsFor 按组件分发，
// 数据袋拆分后不存在"继承别的组件 extraArgs"的可能。
func ControlPlaneFlags(c *Context, component string) (any, error) {
	switch component {
	case constants.ComponentAPIServer:
		return APIServerFlags(c)
	case constants.ComponentControllerManager:
		return ControllerManagerFlags(c)
	case constants.ComponentScheduler:
		return SchedulerFlags(c)
	case constants.ComponentEtcd:
		return EtcdFlags(c)
	default:
		return nil, fmt.Errorf("render: 未知控制面组件 %q", component)
	}
}

// etcdServers 返回本集群 apiserver 的 --etcd-servers 取值。
// 三种形态：
// - stacked：本机 etcd（127.0.0.1）；
// - external + unmanaged：用户提供的 endpoints；
// - external：直接使用用户提供的 endpoints
// endpoints 必须为空，因此这里不能只读 endpoints，否则 apiserver 会拿到
// 空的 --etcd-servers 而起不来。
func etcdServers(c *Context) []string {
	if c.Cluster == nil {
		return nil
	}
	spec := c.Cluster
	if !spec.ControlPlane.Etcd.IsExternal() {
		return []string{fmt.Sprintf("https://127.0.0.1:%d", constants.EtcdClientPort)}
	}
	if len(spec.ControlPlane.Etcd.External.Endpoints) > 0 {
		return append([]string(nil), spec.ControlPlane.Etcd.External.Endpoints...)
	}
	return nil
}

// cgroupDriver 返回 kubelet 必须使用的 cgroup driver。
// 这里重复 internal/runtime 的同一判断而不 import 它：internal/runtime 依赖 internal/render
// 渲染配置文件，反向 import 会成环。判断本身只有几行，重复的代价远小于环。
//
// 三种来源必须一致，否则 containerd 用 cgroupfs 而 kubelet 声称 systemd：
//  1. 自定义 CRI：由 cluster.yaml 的 cgroupDriver 显式给出；
//  2. 托管 containerd：跟随 systemdCgroup（false → cgroupfs）；
//  3. 默认 systemd。
func cgroupDriver(spec *clusterconfig.Cluster) string {
	if spec.Node.Runtime.IsCustom() {
		// custom 模式下 containerd 段不生效（文档明确承诺），因此不能回落到
		// containerd.systemdCgroup；空值表示 kubelet 使用 systemd。
		if d := spec.Node.Runtime.Custom.CgroupDriver; d != "" {
			return d
		}
		return "systemd"
	}
	if sc := spec.Node.Runtime.Containerd.SystemdCgroup; sc != nil && !*sc {
		return "cgroupfs"
	}
	return "systemd"
}

// runtimeEndpoint 返回 kubelet 必须对接的 CRI socket。
func runtimeEndpoint(spec *clusterconfig.Cluster) string {
	if spec.Node.Runtime.IsCustom() {
		if sock := spec.Node.Runtime.Custom.CRISocket; sock != "" {
			return sock
		}
	}
	return "unix:///run/containerd/containerd.sock"
}

// renderControlPlaneComponent 把一个控制面组件渲染成 systemd unit。
func renderControlPlaneComponent(c *Context, spec *Spec, comp ControlPlaneComponent) error {
	p, err := LoadProfile(c)
	if err != nil {
		return err
	}
	compData, err := ControlPlaneFlags(c, comp.Name)
	if err != nil {
		return err
	}
	// flagsCommoner 与 profile.flagsCommon 同构：CommonFlags 以指针嵌入时，
	// 渲染分支在这里统一补 Binary/ExtraArgs（ExtraArgs 不会串组件）。
	if common, ok := compData.(flagsCommoner); ok {
		common.FlagsCommon().Binary = "/usr/local/bin/" + comp.Name
		common.FlagsCommon().ExtraArgs = ComponentExtraArgs(c, comp.Name)
	}

	flags, err := p.RenderFlags(comp.Name, compData)
	if err != nil {
		return err
	}
	unit := SystemdUnit{
		Name:        comp.Name + ".service",
		Description: "kubemono managed " + comp.Name,
		ExecStart:   strings.Join(flags, " "),
		Restart:     "always",
		RestartSec:  "5",
		Wants:       []string{"network-online.target"},
		After:       []string{"network-online.target"},
	}
	spec.AddFile(
		c.Paths.Resolve("/etc/systemd/system/"+unit.Name),
		[]byte(unit.Render()),
		0o644,
		comp.Name,
		KindUnit,
		ActionRestart,
	)
	return nil
}

// ComponentExtraArgs 返回某个组件的运维方配置 flag。
// 导出给 internal/phase/etcd 等复用 profile.FlagsData 的调用方：每个组件必须自己设置
// ExtraArgs，不能继承 FlagsData 里别的组件的值。
// ComponentExtraArgs 是 render 侧取组件 extraArgs 的唯一入口。
// 组件清单由 clusterconfig.ExtraArgsSites 统一维护（校验与渲染同一张表）；
// 保留字（kubemono 自己管理的 flag）已在配置校验阶段拒绝，这里只负责取出。
func ComponentExtraArgs(c *Context, component string) map[string]string {
	if c == nil || c.Cluster == nil {
		return nil
	}
	return c.Cluster.ExtraArgsFor(component)
}

// renderControlPlane 渲染本节点的所有控制面组件。
func renderControlPlane(c *Context, spec *Spec) error {
	// apiserver 的 --audit-policy-file 指向这个文件：它必须先于 apiserver 就位，
	// 否则 apiserver 起不来。
	apiData, err := APIServerFlags(c)
	if err != nil {
		return err
	}
	if apiData.AuditEnabled {
		policy := []byte(apiData.AuditPolicy)
		spec.AddFile(
			apiData.AuditPolicyFile,
			policy,
			0o600,
			constants.ComponentAPIServer,
			KindConfig,
			// 审计策略只在 apiserver 启动时读取，变更必须重启。
			ActionRestart,
		)
	}
	for _, comp := range ControlPlaneComponents() {
		if err := renderControlPlaneComponent(c, spec, comp); err != nil {
			return err
		}
	}
	return nil
}

// etcdInitialClusterList 渲染 stacked etcd 的 --initial-cluster 取值。
// 返回按名字排序的确定性列表。
// **init 节点只列自己**：init 节点用 --initial-cluster-state=new 自建集群，而 etcd
// 的 quorum 要求多数成员在线。列表里如果包含还没启动的成员，这个成员永远选不出
// leader（真机实测：3 成员只起 1 个时 raft 反复 "starting a new election at
// term 1"，gRPC Status 一直超时），etcd phase 会等到超时失败，集群根本建不起来。
// 其余成员由 etcd phase 的 AddLearner 路径加入（internal/phase/etcd/join.go），
// 对应 install 的"join 方等待其他 master 先把集群建立起来"。
// 加入方渲染完整成员列表，并配 --initial-cluster-state=existing（见下方
// control-plane 渲染）：成员已经由 etcd phase 的 AddLearner 加进集群，etcd 启动时
// 只要按这份列表加入即可；写成 new 会让它自建集群（真机实测的集群分裂）。
func etcdInitialClusterList(c *Context) []string {
	if c.Cluster == nil || c.Host == nil {
		return nil
	}
	// 成员来源必须是"真正跑 etcd 的那些主机"：stacked 是 master，
	// etcd 成员只来自 master（堆叠模式）；external 模式不渲染 etcd。
	members := c.Cluster.Cluster.Hosts.Masters()
	self := fmt.Sprintf("%s=https://%s:%d", c.NodeName(), c.Host.Address, constants.EtcdPeerPort)
	if c.IsInitNode {
		return []string{self}
	}
	out := make([]string, 0, len(members))
	for _, m := range members {
		name := m.NodeNameOrName()
		if m.Name == c.Host.Name {
			name = c.NodeName()
		}
		out = append(out, fmt.Sprintf("%s=https://%s:%d", name, m.Address, constants.EtcdPeerPort))
	}
	sort.Strings(out)
	return out
}
