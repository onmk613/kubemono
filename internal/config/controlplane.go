package config

import "kubemono/internal/constants"

// PKISpec 配置证书颁发机构。
type PKISpec struct {
	KeyAlgorithm         string         `json:"keyAlgorithm,omitempty" yaml:"keyAlgorithm,omitempty"`
	CAValidity           Duration       `json:"caValidity,omitempty" yaml:"caValidity,omitempty"`
	CertValidity         Duration       `json:"certValidity,omitempty" yaml:"certValidity,omitempty"`
	ExternalCA           ExternalCASpec `json:"externalCA,omitempty" yaml:"externalCA,omitempty"`
	ExternalFrontProxyCA ExternalCASpec `json:"externalFrontProxyCA,omitempty" yaml:"externalFrontProxyCA,omitempty"`
	ExternalEtcdCA       ExternalCASpec `json:"externalEtcdCA,omitempty" yaml:"externalEtcdCA,omitempty"`
}

// ExternalCASpec 允许导入企业 CA。
type ExternalCASpec struct {
	CertFile string `json:"certFile,omitempty" yaml:"certFile,omitempty"`
	KeyFile  string `json:"keyFile,omitempty" yaml:"keyFile,omitempty"`
}

// EtcdSpec 配置 etcd。
// 没有 type：external 任一字段非空即视为外部 etcd，否则使用本地 etcd。
type EtcdSpec struct {
	Local    LocalEtcdSpec    `json:"local,omitempty" yaml:"local,omitempty"`
	External ExternalEtcdSpec `json:"external,omitempty" yaml:"external,omitempty"`
}

// IsExternal 报告是否使用外部 etcd。
func (s EtcdSpec) IsExternal() bool { return s.External.Configured() }

// Mode 返回 local 或 external，用于状态/选择物料。
func (s EtcdSpec) Mode() string {
	if s.IsExternal() {
		return EtcdExternal
	}
	return EtcdLocal
}

// LocalEtcdSpec 是 kubemono 托管的 etcd。
type LocalEtcdSpec struct {
	DataDir   string            `json:"dataDir,omitempty" yaml:"dataDir,omitempty"`
	ExtraArgs map[string]string `json:"extraArgs,omitempty" yaml:"extraArgs,omitempty"`
}

// ExternalEtcdSpec 描述外部 etcd 集群。
type ExternalEtcdSpec struct {
	Endpoints []string `json:"endpoints,omitempty" yaml:"endpoints,omitempty"`
	CAFile    string   `json:"caFile,omitempty" yaml:"caFile,omitempty"`
	CertFile  string   `json:"certFile,omitempty" yaml:"certFile,omitempty"`
	KeyFile   string   `json:"keyFile,omitempty" yaml:"keyFile,omitempty"`
}

// Configured 报告用户是否填写了 external etcd 字段。
func (s ExternalEtcdSpec) Configured() bool {
	return len(s.Endpoints) > 0 || s.CAFile != "" || s.CertFile != "" || s.KeyFile != ""
}

// ControlPlaneSpec 汇总控制平面、PKI、etcd、组件参数与访问入口。
type ControlPlaneSpec struct {
	PKI               PKISpec       `json:"pki,omitempty" yaml:"pki,omitempty"`
	Etcd              EtcdSpec      `json:"etcd,omitempty" yaml:"etcd,omitempty"`
	Audit             AuditSpec     `json:"audit,omitempty" yaml:"audit,omitempty"`
	APIServer         ExtraArgsSpec `json:"apiServer,omitempty" yaml:"apiServer,omitempty"`
	ControllerManager ExtraArgsSpec `json:"controllerManager,omitempty" yaml:"controllerManager,omitempty"`
	Scheduler         ExtraArgsSpec `json:"scheduler,omitempty" yaml:"scheduler,omitempty"`
	// ExternalEndpoints 是集群稳定外部入口（VIP/DNS）；非空时 worker 直接使用，
	// 不部署本地 haproxy。kubelet 只使用列表中的第一个地址。
	ExternalEndpoints []string `json:"externalEndpoints,omitempty" yaml:"externalEndpoints,omitempty"`
	CertSANs          []string `json:"certSANs,omitempty" yaml:"certSANs,omitempty"`
}

// ExtraArgsSpec 承载用户提供的组件参数。
type ExtraArgsSpec struct {
	ExtraArgs map[string]string `json:"extraArgs,omitempty" yaml:"extraArgs,omitempty"`
}

// LocalProxyEnabled 报告 kubemono 是否应部署 worker 上的 haproxy。
// externalEndpoints 表示集群已有稳定的外部入口（VIP/DNS），worker 直接使用它；
// 此时不需要每台 worker 维护 haproxy。
func (s ControlPlaneSpec) LocalProxyEnabled() bool {
	return len(s.ExternalEndpoints) == 0
}

// AuditSpec 配置 apiserver 审计。
// Policy 为空时使用内置默认策略；Enabled 缺省为 true，保持安全默认。
type AuditSpec struct {
	Enabled      *bool `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	LogMaxAge    *int  `json:"logMaxAge,omitempty" yaml:"logMaxAge,omitempty"`
	LogMaxBackup *int  `json:"logMaxBackup,omitempty" yaml:"logMaxBackup,omitempty"`
	LogMaxSize   *int  `json:"logMaxSize,omitempty" yaml:"logMaxSize,omitempty"`
	// Policy 是完整的 audit.k8s.io/v1 Policy YAML；为空时使用内置默认策略。
	Policy string `json:"policy,omitempty" yaml:"policy,omitempty"`
}

// IsEnabled 报告审计是否开启；未显式配置时默认开启。
func (s AuditSpec) IsEnabled() bool {
	return s.Enabled == nil || *s.Enabled
}

// MaxAge 返回审计日志保留天数。
func (s AuditSpec) MaxAge() int {
	if s.LogMaxAge == nil {
		return constants.DefaultAuditMaxAge
	}
	return *s.LogMaxAge
}

// MaxBackup 返回审计日志最大备份数。
func (s AuditSpec) MaxBackup() int {
	if s.LogMaxBackup == nil {
		return constants.DefaultAuditMaxBackup
	}
	return *s.LogMaxBackup
}

// MaxSize 返回单个审计日志文件的最大 MiB 数。
func (s AuditSpec) MaxSize() int {
	if s.LogMaxSize == nil {
		return constants.DefaultAuditMaxSize
	}
	return *s.LogMaxSize
}
