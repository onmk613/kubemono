package certrenew

import (
	"context"
	"time"

	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	"kubemono/internal/util/output"
)

// Restarter 重启受证书续期影响的组件。
// 生产实现：控制面用 systemctl restart，kube-router/local-proxy 用改写
// restarted-at 注解。测试注入内存实现。
type Restarter interface {
	// Restart 重启给定组件，并在它们健康后返回。
	Restart(ctx context.Context, components []string) error
}

// Options 是每个入口共用的输入。
type Options struct {
	// Paths 解析所有约定路径（生产环境 Root 为空）。
	Paths constants.Paths
	// Cluster 是本地集群配置（可用时）。
	Cluster *clusterconfig.Cluster
	// Host 是配置中本机的条目。
	Host *clusterconfig.Host
	// WarnBefore 是续期窗口；零值使用 constants.DefaultCertWarnBefore。
	WarnBefore time.Duration
	// Validity 是重签证书的有效期；零值先取集群配置，再回退到
	Validity time.Duration
	// CertIDs 只处理指定证书（空表示全部）。
	// 值可以是 catalogue ID、证书文件名或去掉 .crt 的名字。
	CertIDs []string
	// Force 强制重签每个证书，包括仍然有效的。
	Force bool
	// DryRun 只报告将要续期的内容，不写任何文件。
	DryRun bool
	// Report 接收进度消息。
	Report output.Reporter
	// Restarter 重启受影响的组件；为 nil 时只报告命令。
	Restarter Restarter
	// Now 可注入，便于测试确定化。
	Now func() time.Time
}

// Reporter 返回非 nil 的 reporter。
func (o *Options) Reporter() output.Reporter {
	if o == nil || o.Report == nil {
		return output.Discard()
	}
	return o.Report
}

// warnBefore 返回生效的续期窗口。
func (o *Options) warnBefore() time.Duration {
	if o != nil && o.WarnBefore > 0 {
		return o.WarnBefore
	}
	return constants.DefaultCertWarnBefore
}

// validity 返回生效的叶子证书有效期。
func (o *Options) validity() time.Duration {
	if o != nil && o.Validity > 0 {
		return o.Validity
	}
	if o != nil && o.Cluster != nil {
		if v := o.Cluster.CertValidity(); v > 0 {
			return v
		}
	}
	return constants.DefaultCertTTL
}

// now 返回注入的时钟。
func (o *Options) now() time.Time {
	if o != nil && o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// nodeName 返回本机节点名（已知时）。
func (o *Options) nodeName() string {
	switch {
	case o == nil:
		return ""
	case o.Host != nil:
		return o.Host.Name
	default:
		return ""
	}
}

// CertificateResult 是单个证书的处理结果。
type CertificateResult struct {
	// Name 是文件名，例如 apiserver.crt。
	Name string `json:"name"`
	// Path 是证书文件。
	Path string `json:"path"`
	// KeyPath 是私钥文件。
	KeyPath string `json:"keyPath,omitempty"`
	// CommonName 是证书主题 CN。
	CommonName string `json:"commonName,omitempty"`
	// Issuer 是签发 CA 的 CN。
	Issuer string `json:"issuer,omitempty"`
	// OldNotAfter 是原有到期时间。
	OldNotAfter time.Time `json:"oldNotAfter,omitempty"`
	// NewNotAfter 是新的到期时间（续期成功后）。
	NewNotAfter time.Time `json:"newNotAfter,omitempty"`
	// Renewed 报告证书是否被重签。
	Renewed bool `json:"renewed"`
	// Skipped 说明仍然有效的证书为何未被处理。
	Skipped string `json:"skipped,omitempty"`
	// Fingerprint 是新证书的 sha256（DER）。
	Fingerprint string `json:"fingerprint,omitempty"`
	// Error 记录该证书的失败原因。
	Error string `json:"error,omitempty"`
}

// Report 是一次本机续期的结果。
type Report struct {
	// Node 是执行续期的节点。
	Node string `json:"node"`
	// Results 按目录顺序列出每个被检查的证书。
	Results []CertificateResult `json:"results"`
	// Renewed 是重签的证书数量。
	Renewed int `json:"renewed"`
	// Skipped 是仍然有效而未处理的证书数量。
	Skipped int `json:"skipped"`
	// Failed 是续期失败的证书数量。
	Failed int `json:"failed"`
	// Restarted 列出续期后重启的组件。
	Restarted []string `json:"restarted,omitempty"`
	// CAFingerprints 把 CA 名称映射到其 sha256（续期时不得改变）。
	CAFingerprints map[string]string `json:"caFingerprints,omitempty"`
	// Notes 记录运维必须了解的语义。
	Notes []string `json:"notes,omitempty"`
	// DryRun 报告是否有写入。
	DryRun bool `json:"dryRun"`

	// failures 收集续期过程中的错误（不序列化）：
	// Local 收尾时用 joinErrors 合并成返回值，避免每个失败点重复写
	// “Failed++ / Results append / failures append”三元组。
	failures []error
}

// fail 记录一条续期失败：失败计数、结果条目与错误一并入账。
func (r *Report) fail(result CertificateResult, err error) {
	r.Failed++
	r.Results = append(r.Results, result)
	r.failures = append(r.failures, err)
}

// CertStatus 是过期扫描中的一个条目。
// 同一张证书被 PKI 与多个 kubeconfig 引用时聚合成一条：Sources 记录覆盖到的
// 来源，Kubeconfigs 记录引用它的 kubeconfig。
type CertStatus struct {
	// Path 是证书文件（或 kubeconfig 内嵌条目，形如 <kubeconfig>#<user>）。
	Path string `json:"path"`
	// Name 是文件或 kubeconfig 条目名。
	Name string `json:"name"`
	// Source 是聚合后的主要来源：pki / etcd-pki / kubeconfig（按优先级取第一个）。
	Source string `json:"source"`
	// Sources 是覆盖到这张证书的全部来源。
	Sources []string `json:"sources,omitempty"`
	// Kubeconfigs 是引用这张证书的 kubeconfig 文件（绝对路径，已排序去重）。
	Kubeconfigs []string `json:"kubeconfigs,omitempty"`
	// Mismatch 非空表示该条目与 kubemono 期望不一致（引用了别的路径、内嵌证书
	// 与 PKI 文件不同、集群 CA 与本机 ca.crt 不同）。
	Mismatch string `json:"mismatch,omitempty"`
	// CommonName 是主题 CN。
	CommonName string `json:"commonName,omitempty"`
	// Issuer 是签发者 CN。
	Issuer string `json:"issuer,omitempty"`
	// IsCA 报告证书是否为 CA。
	IsCA bool `json:"isCA,omitempty"`
	// NotAfter 是到期时间。
	NotAfter time.Time `json:"notAfter,omitempty"`
	// Remaining 是剩余有效期（人类可读）。
	Remaining string `json:"remaining,omitempty"`
	// Expired 报告证书是否已过期。
	Expired bool `json:"expired"`
	// Warn 报告证书是否已进入续期窗口。
	Warn bool `json:"warn"`
	// Error 记录解析/读取失败。
	Error string `json:"error,omitempty"`

	// fingerprint 是证书 DER 的 sha256，仅用于聚合与一致性比较。
	fingerprint string
	// kubeconfig 是扫描阶段的单条引用来源，聚合后进入 Kubeconfigs。
	kubeconfig string
}
