// Package constants 汇集 kubemono 的全部约定：路径、端口、组件名与默认值。
// 这里是"单一事实来源"：任何一个魔法字符串只应该出现在本包，其他包引用常量，
// 这样改一处即可全局生效（例如 /etc/kubernetes 的根目录）。
package constants

import (
	"slices"
	"time"
)

const (
	// KubernetesDir 是 Kubernetes 配置目录树的根。
	KubernetesDir = "/etc/kubernetes"
	// PKIDir 存放 CA 与叶子证书。
	PKIDir = KubernetesDir + "/pki"
	// EtcdPKIDir 存放本地 etcd 的 PKI。
	EtcdPKIDir = PKIDir + "/etcd"
	// ManifestsDir 存放生效中的静态 Pod 清单。
	ManifestsDir = KubernetesDir + "/manifests"
	// TmpDir 用于原子替换清单。
	TmpDir = KubernetesDir + "/tmp"
	// EncryptionConfigDir 存放 Secret 加密配置。
	EncryptionConfigDir = KubernetesDir + "/encryption"
	// AuditDir 存放 apiserver 审计策略（在静态 Pod 内为只读）。
	AuditDir = KubernetesDir + "/audit"
	// AuditLogDir 是 apiserver 写入审计日志的位置。
	// 必须是**可写**路径：静态 Pod 只把 /var/log/kubernetes 挂成可写
	// （kube-apiserver 清单的 audit-logs 卷），/etc/kubernetes 是只读挂载。
	// 真机实测：日志跟着策略写进 /etc/kubernetes/audit 时 apiserver 直接
	// "ensureLogFile: open /etc/kubernetes/audit/audit.log: read-only file system" 退出。
	AuditLogDir = "/var/log/kubernetes"
	// KubeletDir 是 kubelet 的状态目录。
	KubeletDir = "/var/lib/kubelet"

	// KubemonoDir 是 kubemono 的默认数据目录；测试根或 --data-dir 可以重定位它。
	KubemonoDir = "/usr/local/kubemono"
	// NodeConfigFile 是本机节点身份。
	NodeConfigFile = KubemonoDir + "/node.yaml"
	// ClusterConfigFile 是集群配置的本机缓存。
	ClusterConfigFile = KubemonoDir + "/cluster.yaml"
	// DesiredDir 存放本节点渲染后的期望状态。
	DesiredDir = KubemonoDir + "/desired"
	// ArtifactsDir 存放导入的物料（离线安装与修复源）。
	ArtifactsDir = KubemonoDir + "/artifacts"
	// StateDir 存放 phase 进度与运行时状态。
	StateDir = KubemonoDir + "/state"
	// BackupDir 存放变更前备份。
	BackupDir    = KubemonoDir + "/backup"
	EtcdDataDir  = "/var/lib/etcd"
	CNIConfigDir = "/etc/cni/net.d"
	CNIBinDir    = "/opt/cni/bin"
	CNIDataDir   = "/var/lib/cni"
)

// PKI 与 kubeconfig 目录内的固定文件名。
const (
	CACertName                     = "ca.crt"
	CAKeyName                      = "ca.key"
	FrontProxyCACertName           = "front-proxy-ca.crt"
	FrontProxyCAKeyName            = "front-proxy-ca.key"
	FrontProxyClientCertName       = "front-proxy-client.crt"
	FrontProxyClientKeyName        = "front-proxy-client.key"
	APIServerCertName              = "apiserver.crt"
	APIServerKeyName               = "apiserver.key"
	APIServerKubeletClientCertName = "apiserver-kubelet-client.crt"
	APIServerKubeletClientKeyName  = "apiserver-kubelet-client.key"
	APIServerEtcdClientCertName    = "apiserver-etcd-client.crt"
	EtcdServerCertName             = "server.crt"
	EtcdServerKeyName              = "server.key"
	EtcdPeerCertName               = "peer.crt"
	EtcdPeerKeyName                = "peer.key"
	APIServerEtcdClientKeyName     = "apiserver-etcd-client.key"
	ServiceAccountPubName          = "sa.pub"
	ServiceAccountKeyName          = "sa.key"

	// 客户端身份证书：kubeconfig 引用它们，而不是把证书嵌进文件里，
	// 这样证书轮换不影响 kubeconfig 内容，也不会让组件重启两次。
	AdminClientCertName             = "admin.crt"
	AdminClientKeyName              = "admin.key"
	SuperAdminClientCertName        = "super-admin.crt"
	SuperAdminClientKeyName         = "super-admin.key"
	ControllerManagerClientCertName = "controller-manager.crt"
	ControllerManagerClientKeyName  = "controller-manager.key"
	SchedulerClientCertName         = "scheduler.crt"
	SchedulerClientKeyName          = "scheduler.key"
	KubeletClientCertName           = "kubelet-client.crt"
	KubeletClientKeyName            = "kubelet-client.key"
	// 纯 worker 没有 CA 私钥，继续使用 kubelet TLS Bootstrap 产生的 kubelet.conf。

	SuperAdminKubeconfigName        = "super-admin.conf"
	AdminKubeconfigName             = "admin.conf"
	ControllerManagerKubeconfigName = "controller-manager.conf"
	SchedulerKubeconfigName         = "scheduler.conf"
	KubeletKubeconfigName           = "kubelet.conf"
	BootstrapKubeletKubeconfigName  = "bootstrap-kubelet.conf"
	EtcdKubemonoClientCertName      = "kubemono-client.crt"
	EtcdKubemonoClientKeyName       = "kubemono-client.key"
	EncryptionConfigName            = "config.yaml"
	AuditPolicyName                 = "policy.yaml"

	// KubeconfigAdminPath 是默认的集群级凭据路径。
	KubeconfigAdminPath = KubernetesDir + "/" + AdminKubeconfigName

	// GroupClusterAdmins 是 admin.conf 使用的组。
	GroupClusterAdmins = "kubemono:cluster-admins"
	// UserAPIServerKubeletClient 是 apiserver kubelet 客户端证书的 CN。
	UserAPIServerKubeletClient = "kubemono:apiserver-kubelet-client"
	// UserEtcdClient 是 kubemono etcd 客户端证书的 CN。
	UserEtcdClient = "kubemono-etcd-client"
	// UserAdmin 是 admin.conf 的 CN；它通过 RBAC 授权，因此可以被吊销，
	// 这一点与 super-admin.conf 不同。
	UserAdmin = "kubemono-admin"
	// UserSuperAdmin 是 super-admin.conf 的 CN（O=system:masters）。
	UserSuperAdmin = "kubernetes-admin"
	// UserControllerManager 是 controller-manager.conf 的 CN。
	UserControllerManager = "system:kube-controller-manager"
	// UserScheduler 是 scheduler.conf 的 CN。
	UserScheduler = "system:kube-scheduler"
	// UserKubeletBootstrap 是 bootstrap kubelet kubeconfig 的 CN。
	UserKubeletBootstrap = "kubelet-bootstrap"
	// GroupSystemNodes 是所有 kubelet 客户端证书所属的组。
	GroupSystemNodes = "system:nodes"
	// GroupSystemMasters 通过内置绑定授予 cluster-admin。
	GroupSystemMasters = "system:masters"
)

// 集群级对象名称与命名空间。
// kubemono 不单独占一个命名空间：集群身份与 join 物料都放在 kube-system。
const (
	ClusterStateName = "kubemono-cluster"

	BootstrapTokenSecretPrefix = "bootstrap-token-"
	ClusterInfoName            = "cluster-info"
	KubePublicNamespace        = "kube-public"
	KubeSystemNamespace        = "kube-system"

	// Bootstrap 组名称。
	GroupBootstrappersWorker = "system:bootstrappers:kubemono:worker"
	GroupBootstrappersMaster = "system:bootstrappers:kubemono:master"
)

// 端口。
const (
	APIServerPort         = 6443
	EtcdClientPort        = 2379
	EtcdPeerPort          = 2380
	EtcdMetricsPort       = 2381
	KubeletPort           = 10250
	KubeletHealthzPort    = 10248
	ControllerManagerPort = 10257
	SchedulerPort         = 10259
	KubeRouterHealthzPort = 20244
	BGPPort               = 179
	KubeProxyMetricsPort  = 10249
	KubeProxyHealthzPort  = 10256
)

// 控制平面组件名称。
const (
	ComponentEtcd              = "etcd"
	ComponentAPIServer         = "kube-apiserver"
	ComponentControllerManager = "kube-controller-manager"
	ComponentScheduler         = "kube-scheduler"
	ComponentKubelet           = "kubelet"
	ComponentKubeRouter        = "kube-router"
	ComponentKubeProxy         = "kube-proxy"
	ComponentLocalProxy        = "local-proxy"
	ComponentCoreDNS           = "coredns"
	ComponentKubectl           = "kubectl"
)

// kubernetesComponentOrder 是 k8s 组件（控制面三件套 + kubelet +
// kube-proxy + kubectl）的规范顺序。
var kubernetesComponentOrder = []string{
	ComponentAPIServer,
	ComponentControllerManager,
	ComponentScheduler,
	ComponentKubelet,
	ComponentKubeProxy,
	ComponentKubectl,
}

// KubernetesComponentNames 返回全部 k8s 组件名（副本，调用方可安全排序）。
func KubernetesComponentNames() []string {
	return append([]string(nil), kubernetesComponentOrder...)
}

// IsKubernetesComponent 报告 name 是否是 Kubernetes 控制平面组件，
// 其版本由 BOM 的 kubernetes 版本固定。
func IsKubernetesComponent(name string) bool { return slices.Contains(kubernetesComponentOrder, name) }

// 配置字段为空时使用的默认值。
const (
	DefaultClusterDNS             = "10.96.0.10"
	DefaultPodCIDR                = "10.244.0.0/16"
	DefaultServiceCIDR            = "10.96.0.0/12"
	DefaultDNSDomain              = "cluster.local"
	DefaultNodeCIDRMaskSize int32 = 24
	DefaultLocalProxyPort         = 6443
	DefaultInstallWait            = 30 * time.Minute
	DefaultCertWarnBefore         = 30 * 24 * time.Hour
	DefaultCATTL                  = 10 * 365 * 24 * time.Hour
	DefaultCertTTL                = 365 * 24 * time.Hour
	// DefaultBootstrapTokenTTL 是 join 凭证里 bootstrap token 的默认有效期。
	// 采用短期凭证模型：令牌过期后必须重新执行 token create。
	DefaultBootstrapTokenTTL = 2 * time.Hour
	// MaxBootstrapTokenTTL 是 bootstrap token 的硬上限。
	// 任何超过该值的 TTL 都会在创建 Secret 时被拒绝，避免凭证长期有效。
	MaxBootstrapTokenTTL  = 2 * time.Hour
	DefaultAuditMaxAge    = 7
	DefaultAuditMaxBackup = 10
	DefaultAuditMaxSize   = 100
	// DefaultLocalBinDir 是运行二进制（真实文件）的安装目录。
	DefaultLocalBinDir = "/usr/local/bin"
	// APIServerCertCN 是 apiserver 服务端证书的 CN。
	APIServerCertCN = "kube-apiserver"
)
