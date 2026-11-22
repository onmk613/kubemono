package etcd

import (
	"fmt"
	"strings"

	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
)

// Form 是 etcd 的部署形态。
type Form string

// 部署形态。
const (
	// FormStacked 是默认形态：etcd 随 master 一起部署。
	FormStacked Form = "stacked"
	// FormExternalUnmanaged 是 external + unmanaged：只引用 endpoints 和证书。
	FormExternalUnmanaged Form = "external-unmanaged"
)

// Provider 把 的形态差异收敛到一处。
// 端点、证书、本机成员与配置里的成员表都从这里取；
// phase 与 CLI 不判断 controlPlane.etcd.type。
type Provider interface {
	// Form 返回部署形态。
	Form() Form
	// Local 描述本机 etcd 成员。
	Local() Local
	// ClientConfig 返回访问整个 etcd 集群的参数。
	ClientConfig() (Config, error)
	// MemberConfig 返回成员变更使用的参数：端点优先排除本机
	// （加入者的 etcd 还没起来，把本机放在列表里只会拖慢故障转移）。
	MemberConfig() (Config, error)
	// ConfiguredNames 返回配置里声明的 etcd 成员名。
	ConfiguredNames() []string
}

// NewProvider 根据集群配置与本机身份构造 Provider。
// host 可以为 nil（准备机、或本机还没登记）：只读命令仍能从集群配置得到
// endpoints 与证书路径；与“本机成员”相关的判断一律为 false。
func NewProvider(cluster *clusterconfig.Cluster, host *clusterconfig.Host, paths constants.Paths, localNodeName ...string) (Provider, error) {
	if cluster == nil {
		return nil, fmt.Errorf("etcd: 缺少集群配置，无法确定 etcd 形态")
	}
	base := providerBase{cluster: cluster, host: host, paths: paths}
	if len(localNodeName) > 0 {
		base.nodeName = localNodeName[0]
	}
	if cluster.ControlPlane.Etcd.IsExternal() {
		return &unmanagedProvider{base}, nil
	}
	return &stackedProvider{base}, nil
}

// ServiceName 是本机 etcd 的 systemd unit 名（仅 stacked）。
const ServiceName = "etcd.service"

// providerBase 是三种形态共用的数据。
type providerBase struct {
	cluster  *clusterconfig.Cluster
	host     *clusterconfig.Host
	paths    constants.Paths
	nodeName string
}

// dataDir 返回本机 etcd 数据目录。
func (b providerBase) dataDir() string {
	if b.cluster.ControlPlane.Etcd.Local.DataDir != "" {
		return b.cluster.ControlPlane.Etcd.Local.DataDir
	}
	return constants.EtcdDataDir
}

// localPKI 返回 stacked 使用的 kubemono 客户端证书与 etcd CA。
func (b providerBase) localPKI() TLSFiles {
	return TLSFiles{
		CAFile:   b.paths.EtcdCertPath(constants.CACertName),
		CertFile: b.paths.EtcdCertPath(constants.EtcdKubemonoClientCertName),
		KeyFile:  b.paths.EtcdCertPath(constants.EtcdKubemonoClientKeyName),
	}
}

// local 组装本机成员信息。
func (b providerBase) local(member bool) Local {
	local := Local{
		Name:        b.hostName(),
		Member:      member,
		DataDir:     b.dataDir(),
		ServiceName: ServiceName,
	}
	if !member {
		return local
	}
	addr := b.host.Address
	local.PeerURLs = PeerURLs([]string{addr})
	local.ClientURL = ClientURLs([]string{addr})[0]
	return local
}

// hostName 返回本机在配置里的名字。
func (b providerBase) hostName() string {
	if b.nodeName != "" {
		return b.nodeName
	}
	if b.host == nil {
		return ""
	}
	return b.host.NodeNameOrName()
}

// clientConfig 构造客户端参数。
// excludeSelf 为真时优先剔除指向本机地址的 endpoint；若剔除后为空则保留原列表
// （单节点集群的成员操作只能落在本机）。
func (b providerBase) clientConfig(form Form, endpoints []string, tls TLSFiles, excludeSelf bool) (Config, error) {
	if err := tls.Validate(); err != nil {
		return Config{}, err
	}
	if excludeSelf && b.host != nil && b.host.Address != "" {
		filtered := make([]string, 0, len(endpoints))
		for _, e := range endpoints {
			if !strings.Contains(e, b.host.Address) {
				filtered = append(filtered, e)
			}
		}
		if len(filtered) > 0 {
			endpoints = filtered
		} else {
			// 单节点集群：成员操作只能落回本机；用 127.0.0.1 而不依赖网卡地址。
			endpoints = []string{fmt.Sprintf("https://127.0.0.1:%d", constants.EtcdClientPort)}
		}
	}
	if len(endpoints) == 0 {
		return Config{}, fmt.Errorf("etcd: %s 形态没有可用的 endpoints：stacked 需要 master 角色，external 需要 controlPlane.etcd.external.endpoints", form)
	}
	return Config{Endpoints: endpoints, TLS: tls}, nil
}

// localFirst 把 127.0.0.1 放在 endpoints 最前（本机是 etcd 成员时）。
// 本机 etcd 一定监听 127.0.0.1:2379，本机优先可以避免单节点集群依赖网卡地址。
func (b providerBase) localFirst(cfg Config, local bool) Config {
	if !local {
		return cfg
	}
	cfg.Endpoints = append([]string{fmt.Sprintf("https://127.0.0.1:%d", constants.EtcdClientPort)}, cfg.Endpoints...)
	return cfg
}

// configuredNames 返回成员名。
func (b providerBase) configuredNames(hosts []clusterconfig.Host) []string {
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, h.NodeNameOrName())
	}
	return out
}

// stackedProvider 是 stacked 形态：etcd 随 master 部署。
type stackedProvider struct{ providerBase }

// Form 实现 Provider。
func (p *stackedProvider) Form() Form { return FormStacked }

// Capabilities 实现 Provider。
// Local 实现 Provider。
// stacked etcd 以 etcd.service 运行。
func (p *stackedProvider) Local() Local {
	member := p.host != nil && p.host.IsMaster()
	return p.local(member)
}

// ClientConfig 实现 Provider。
func (p *stackedProvider) ClientConfig() (Config, error) {
	hosts := p.cluster.Cluster.Hosts.Masters()
	cfg, err := p.clientConfig(FormStacked, clientURLsOf(hosts), p.localPKI(), false)
	if err != nil {
		return cfg, err
	}
	return p.localFirst(cfg, p.Local().Member), nil
}

// MemberConfig 实现 Provider。
func (p *stackedProvider) MemberConfig() (Config, error) {
	hosts := p.cluster.Cluster.Hosts.Masters()
	return p.clientConfig(FormStacked, clientURLsOf(hosts), p.localPKI(), true)
}

// ConfiguredNames 实现 Provider。
func (p *stackedProvider) ConfiguredNames() []string {
	return p.configuredNames(p.cluster.Cluster.Hosts.Masters())
}

// unmanagedProvider 是 external + unmanaged：只引用 endpoints 和证书。
type unmanagedProvider struct{ providerBase }

// Form 实现 Provider。
func (p *unmanagedProvider) Form() Form { return FormExternalUnmanaged }

// Capabilities 实现 Provider。
// Local 实现 Provider：unmanaged 形态下本机永远不运行 etcd 成员。
func (p *unmanagedProvider) Local() Local { return p.local(false) }

// ClientConfig 实现 Provider。
func (p *unmanagedProvider) ClientConfig() (Config, error) {
	ext := p.cluster.ControlPlane.Etcd.External
	tls := TLSFiles{CAFile: ext.CAFile, CertFile: ext.CertFile, KeyFile: ext.KeyFile}
	if tls.Empty() {
		return Config{}, fmt.Errorf("etcd: external+unmanaged 需要 controlPlane.etcd.external.caFile/certFile/keyFile；如证书缺失，先由管理员把证书放到本机")
	}
	return p.clientConfig(FormExternalUnmanaged, append([]string(nil), ext.Endpoints...), tls, false)
}

// MemberConfig 实现 Provider。
func (p *unmanagedProvider) MemberConfig() (Config, error) {
	// 能力矩阵会在调用方被 Require(OpMemberChange) 拦下；这里仍返回可用参数，
	// 便于只读命令与排障。
	return p.ClientConfig()
}

// ConfiguredNames 实现 Provider。
func (p *unmanagedProvider) ConfiguredNames() []string { return nil }

// clientURLsOf 返回一组 host 的 client URL。
func clientURLsOf(hosts []clusterconfig.Host) []string {
	addresses := make([]string, 0, len(hosts))
	for _, h := range hosts {
		addresses = append(addresses, h.Address)
	}
	return ClientURLs(addresses)
}
