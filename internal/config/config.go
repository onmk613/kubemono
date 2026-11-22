// Package config 包含 kubemono 的 cluster.yaml 与 node.yaml 类型、默认值和校验。
// 这些类型只描述 kubemono 自己的配置文件，不是 Kubernetes API 对象。
// 类型本身只依赖标准库；只有 validate.go 借用 apimachinery 的 field.ErrorList
// 来产生 "<字段路径>: Invalid value..." 形式的诊断信息（收益高、体积极小）。
package config

// 运行时模式（由 custom 字段是否配置推导）。
const (
	// RuntimeContainerd 是 kubemono 托管的 containerd 运行时。
	RuntimeContainerd = "containerd"
	// RuntimeCustom 是用户自备的 CRI。
	RuntimeCustom = "custom"
)

// etcd 模式（由 external 字段是否配置推导）。
const (
	// EtcdLocal 让 etcd 作为控制平面的一部分运行。
	EtcdLocal = "local"
	// EtcdExternal 使用外部 etcd 集群。
	EtcdExternal = "external"
)

// 网络插件（network.plugin）。
const (
	// PluginKubeRouter 以 kube-router 作为 CNI 部署。
	PluginKubeRouter = "kube-router"
	// PluginNone 表示由用户自行部署 CNI。
	PluginNone = "none"
)

// kube-proxy 代理模式（network.kubeProxy.mode）。
// kubemono 不内置 kube-router 的 Service 代理，Service 统一由 kube-proxy 承担。
const (
	// KubeProxyModeIPVS 使用 IPVS（默认）。
	KubeProxyModeIPVS = "ipvs"
	// KubeProxyModeIPTables 使用 iptables。
	KubeProxyModeIPTables = "iptables"
	// KubeProxyModeNFTables 使用 nftables；需要节点内核 >= 5.13。
	KubeProxyModeNFTables = "nftables"
	// KubeProxyDefaultScheduler 是 IPVS 默认调度器。
	KubeProxyDefaultScheduler = "rr"
)

// PKI 密钥算法（controlPlane.pki.keyAlgorithm）。
const (
	KeyAlgorithmRSA2048   = "rsa2048"
	KeyAlgorithmECDSAP256 = "ecdsa-p256"
)

// Cluster 是 cluster.yaml 的根对象。
type Cluster struct {
	// KubernetesVersion 是本集群使用的 Kubernetes patch 版本，例如 v1.37.4。
	// 它是 k8s 组件的**唯一**版本入口：全部 k8s 组件整组跟随它，不能单独
	// 在 images.componentVersions 里固定。为空时使用 kubemono 发行版内置的默认值。
	// 任意版本都允许，与默认版本不同时只告警。
	KubernetesVersion string           `json:"kubernetesVersion,omitempty" yaml:"kubernetesVersion,omitempty"`
	Cluster           ClusterSpec      `json:"cluster" yaml:"cluster"`
	Node              NodeSpec         `json:"node" yaml:"node"`
	ControlPlane      ControlPlaneSpec `json:"controlPlane" yaml:"controlPlane"`
	Network           NetworkSpec      `json:"network" yaml:"network"`
	Addons            AddonsSpec       `json:"addons" yaml:"addons"`
	Images            ImagesSpec       `json:"images" yaml:"images"`

	// Warnings 是 Validate 产生的非致命提示（不落盘、不参与比较）。
	// 由 LoadCluster 填充，CLI 统一打印；这样 init/join/repair 也能看到
	// “偶数 etcd 成员”“nftables 内核要求”这类提示，而不只是 config validate。
	Warnings []string `json:"-" yaml:"-"`
}

// ClusterSpec 是集群成员。
// 一个 cluster.yaml 只对应一个集群，集群身份由安装时生成的 clusterID 标识，
// 没有可配置的集群名。
type ClusterSpec struct {
	Hosts Hosts     `json:"hosts" yaml:"hosts"`
	Hooks HooksSpec `json:"hooks,omitempty" yaml:"hooks,omitempty"`
}

// HooksSpec 列出在预定义时点运行的用户钩子。
type HooksSpec struct {
	PreHostInit []string `json:"preHostInit,omitempty" yaml:"preHostInit,omitempty"`
	PostRuntime []string `json:"postRuntime,omitempty" yaml:"postRuntime,omitempty"`
	PostInstall []string `json:"postInstall,omitempty" yaml:"postInstall,omitempty"`
}
