package install

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"kubemono/internal/bootstrap"
	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	"kubemono/internal/discovery"
	"kubemono/internal/join/certs"
	"kubemono/internal/util/fsutil"
)

// prepareJoin 让本机先拿到"别人的东西"：集群 CA（worker）或整个密钥包（master）。
// 这是 判定与 第 6 步之间的桥；只有 join 动作需要取密钥材料——
// sync 是"已安装节点"的本机幂等修复，CA/证书已经在 /etc/kubernetes/pki 上。
// 真机实测：分步安装（只跑前几个 phase）等前置阶段跑完后再跑完整 install
// 会被判成 sync，那时要求 join 凭证是错的（密钥根本不需要重新取）。
// 调用方（Run）只在 plan.Action == ActionJoin 时进入，因此这里不重复判断动作。
func (o *Options) prepareJoin(ctx context.Context) error {
	if o.JoinSecret == nil {
		return fmt.Errorf("加入集群需要 --token：master 还需要 --control-plane 与 --certificate-key；" +
			"如果这是第一台 master，请先执行 kubemono init")
	}
	token, err := bootstrap.ParseToken(o.JoinSecret.Token)
	if err != nil {
		return fmt.Errorf("引导令牌无效: %w", err)
	}

	if err := o.validateJoinRole(); err != nil {
		return err
	}
	if o.isWorkerOnly() {
		return o.retryJoin(ctx, func() error { return o.fetchClusterCA(ctx, token) })
	}
	return o.retryJoin(ctx, func() error { return o.fetchCertificateBundle(ctx, token) })
}

// validateJoinRole 校验 join 凭证与本机角色严格匹配：
// worker 必须是 role=worker 且不带 certificateKey；master 必须是 role=master
// 且带 certificateKey。role 由 --control-plane 决定。
func (o *Options) validateJoinRole() error {
	if o.isWorkerOnly() {
		if o.JoinSecret.Role != clusterconfig.RoleWorker {
			return fmt.Errorf("纯 worker 不能以 master 身份加入：不要使用 --control-plane")
		}
		if o.JoinSecret.CertificateKey != "" {
			return fmt.Errorf("纯 worker 不能携带 certificateKey：不要传 --certificate-key")
		}
		return nil
	}
	if o.JoinSecret.Role != clusterconfig.RoleMaster || o.JoinSecret.CertificateKey == "" {
		return fmt.Errorf("master/etcd 加入需要 --control-plane 与 --certificate-key")
	}
	return nil
}

// retryJoin 等待集群变为可达。只重试传输层失败和已过期的一次性请求；
// JWS/身份错误是确定性的，必须立即失败。
// 在这个循环出现之前，即使 master 已初始化但 apiserver 尚未就绪，
// `install --wait` 也会在第一次 "connection refused" 时失败。
func (o *Options) retryJoin(ctx context.Context, fn func() error) error {
	wait := o.Wait
	if wait <= 0 {
		wait = constants.DefaultInstallWait
	}
	deadline := time.Now().Add(wait)
	var lastErr error
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		lastErr = fn()
		if lastErr == nil {
			return nil
		}
		if !isTransientJoinError(lastErr) {
			return lastErr
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("等待集群加入应答超时（%s）：%w", wait, lastErr)
		}
		if attempt == 0 {
			o.Report.Infof("集群暂不可用，开始等待（最长 %s）：%v", wait, lastErr)
		}
		interval := 5 * time.Second
		if remaining := time.Until(deadline); remaining < interval {
			interval = remaining
		}
		o.Report.Infof("等待集群可用：%v（%s 后重试）", lastErr, interval.Round(time.Second))
		o.Sleep(interval)
	}
}

// isTransientJoinError 报告等待是否可能修复该错误。
func isTransientJoinError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, discovery.ErrUnreachable) {
		return true
	}
	if apierrors.IsServiceUnavailable(err) || apierrors.IsTimeout(err) ||
		apierrors.IsServerTimeout(err) || apierrors.IsTooManyRequests(err) {
		return true
	}
	return discovery.IsUnreachable(err)
}

// isWorkerOnly 报告节点是否只运行工作负载（没有控制面）。
func (o *Options) isWorkerOnly() bool {
	host, ok := o.self()
	if !ok {
		return false
	}
	return !host.IsMaster()
}

// self 返回本节点在配置中的条目。
// 节点名由 Decide 解析后写入 Options.Node，因此这里不需要再猜。
func (o *Options) self() (*clusterconfig.Host, bool) {
	if o.Cluster == nil || o.Node == "" {
		return nil, false
	}
	return o.Cluster.Cluster.Hosts.FindHost(o.Node)
}

// fetchClusterCA 写入目标集群中经 JWS 校验过的 CA。
func (o *Options) fetchClusterCA(ctx context.Context, token bootstrap.Token) error {
	endpoints := o.joinEndpoints()
	if err := validateJoinEndpoints(endpoints); err != nil {
		return err
	}
	info, err := discovery.Probe(ctx, o.discoveryOptions(token, endpoints, 10*time.Second))
	if err != nil {
		return fmt.Errorf("发现集群失败: %w", err)
	}
	if info == nil {
		return fmt.Errorf("没有探测到可加入的集群：请确认 master 已就绪，且 bootstrap token 未过期")
	}
	if err := o.adoptClusterID(info.ClusterID); err != nil {
		return err
	}
	target := o.Paths.CertPath(constants.CACertName)
	if existing, err := os.ReadFile(target); err == nil && string(existing) == string(info.CA) {
		o.Report.Debugf("集群 CA 已是最新：%s", target)
		return nil
	}
	if err := fsutil.WriteFileAtomic(target, info.CA, 0o644); err != nil {
		return fmt.Errorf("写入集群 CA %s: %w", target, err)
	}
	o.Report.Infof("已从 %s 取得并校验集群 CA", info.Endpoint)
	return nil
}

// fetchCertificateBundle 取得集群证书包并写入本机（master 与 etcd 节点）。
// 签发 master 凭证时，kubemono token create --role master 已经把 CA/SA 物料用
// certificateKey 加密写入 kube-system/kubemono-certs；加入者用 bootstrap token
// 读取、用 --certificate-key 解密，再按规范路径落盘。CA 私钥冲突时要求 --replace-pki。
func (o *Options) fetchCertificateBundle(ctx context.Context, token bootstrap.Token) error {
	certKey, err := certs.DecodeKey(o.JoinSecret.CertificateKey)
	if err != nil {
		return fmt.Errorf("certificateKey 无效（master 需要它）: %w", err)
	}
	client, info, err := o.bootstrapClient(ctx, token)
	if err != nil {
		return err
	}
	if info != nil {
		if err := o.adoptClusterID(info.ClusterID); err != nil {
			return err
		}
	}
	ciphertext, err := certs.Download(ctx, client)
	if err != nil {
		return err
	}
	bundle, err := certs.Decrypt(ciphertext, certKey)
	if err != nil {
		return err
	}
	if err := o.adoptClusterID(bundle.ClusterID); err != nil {
		return fmt.Errorf("证书包 clusterID 校验失败: %w", err)
	}
	// 证书包里的 ca.crt 必须与 join 命令的 CA pin 一致：cluster-info 的 CA
	// 已经过 JWS 校验，证书包是另一条通道，两条通道必须指向同一个信任根。
	if err := bootstrap.VerifyCACertHashes(bundle.Files[certs.FileCACert], o.joinCACertHashes()); err != nil {
		return fmt.Errorf("证书包里的 CA 与 --discovery-token-ca-cert-hash 不一致：%w", err)
	}
	if err := bundle.WriteTo(o.Paths, o.ReplacePKI, o.Report.Infof); err != nil {
		return err
	}
	o.Report.Infof("已取得并解密集群证书包（%d 个文件）", len(bundle.Files))
	return nil
}

// bootstrapClient 用引导令牌构造客户端。
// token 只能在“确认对端是目标集群”之后发出去：先用 discovery.Probe 依次尝试
// externalEndpoints、join 凭证里的 discoveryEndpoints 和所有 master；Probe 同时
// 校验 cluster-info 的 JWS 与 caCertHashes。成功后用实际应答的 endpoint 做密封
// 交换，而不是盲选第一台 master。
func (o *Options) bootstrapClient(ctx context.Context, token bootstrap.Token) (kubernetes.Interface, *discovery.Info, error) {
	if o.ClientFactory != nil {
		client, err := o.ClientFactory(token)
		return client, nil, err
	}
	info, err := o.joinServer(ctx, token)
	if err != nil {
		return nil, nil, err
	}
	if len(o.joinCACertHashes()) == 0 {
		o.Report.Warnf("没有 --discovery-token-ca-cert-hash：首跳无法校验服务端证书，" +
			"引导令牌只能依赖后续 JWS 保证安全（建议重新签发 join 命令）")
	}
	cfg, err := joinRESTConfig(info, token)
	if err != nil {
		return nil, nil, err
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, nil, err
	}
	return client, info, nil
}

// adoptClusterID 把发现到的集群身份写入本次 join。fresh join 的 Options.ClusterID
// 为空，之前的 bundle 校验因此永远不生效；现在由 discovery/cluster-info 或证书
// 包提供，并在 node.yaml 里持久化。
func (o *Options) adoptClusterID(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil
	}
	if o.ClusterID != "" && o.ClusterID != id {
		return fmt.Errorf("本机准备加入集群 %s，但发现到的集群是 %s：拒绝混用物料", o.ClusterID, id)
	}
	o.ClusterID = id
	return nil
}

// joinRESTConfig 用 cluster-info 里经 JWS 校验的 CA 构造 TLS 客户端。
// 首跳只能先信任 JWS（此时还没有可信 CA）；拿到 cluster-info 后，后续请求
// 改用这里经 JWS 校验的 CA。apiserver 只发送叶子证书、CA 不在证书链里，
// 所以 caCertHashes 不能直接当服务端证书 pin 使用。
func joinRESTConfig(info *discovery.Info, token bootstrap.Token) (*rest.Config, error) {
	if info == nil || strings.TrimSpace(info.Endpoint) == "" {
		return nil, fmt.Errorf("没有 master 返回可用的 cluster-info")
	}
	if len(info.CA) == 0 {
		return nil, fmt.Errorf("cluster-info 没有携带集群 CA，拒绝加入")
	}
	return &rest.Config{
		Host:        info.Endpoint,
		BearerToken: token.String(),
		Timeout:     30 * time.Second,
		TLSClientConfig: rest.TLSClientConfig{
			CAData: info.CA,
		},
	}, nil
}

// validateJoinEndpoints 拒绝明文 http:// 端点：join 流量携带引导令牌。
func validateJoinEndpoints(endpoints []string) error {
	for _, ep := range endpoints {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(ep)), "http://") {
			return fmt.Errorf("拒绝使用明文 http:// 端点 %q 发起加入：引导令牌会以明文传输，请改用 https://", ep)
		}
	}
	return nil
}

// joinServer 依次探测所有候选端点，返回实际应答且通过校验的那个。
func (o *Options) joinServer(ctx context.Context, token bootstrap.Token) (*discovery.Info, error) {
	endpoints := o.joinEndpoints()
	if len(endpoints) == 0 {
		return nil, fmt.Errorf("集群配置里没有可用的 master 或 externalEndpoints，无法发起加入请求")
	}
	if err := validateJoinEndpoints(endpoints); err != nil {
		return nil, err
	}
	info, err := discovery.Probe(ctx, o.discoveryOptions(token, endpoints, 5*time.Second))
	if err != nil {
		return nil, err
	}
	if info == nil || strings.TrimSpace(info.Endpoint) == "" {
		return nil, fmt.Errorf("没有 master 返回可用的 cluster-info")
	}
	return info, nil
}

// joinEndpoints 汇总 join 时的候选 apiserver 端点：位置参数
// （PreferredEndpoint）最优先，其次是 externalEndpoints、join 凭证里的
// discoveryEndpoints、以及所有 master 地址；去重并保留顺序。
func (o *Options) joinEndpoints() []string {
	var raw []string
	if ep := strings.TrimSpace(o.PreferredEndpoint); ep != "" {
		raw = append(raw, ep)
	}
	raw = append(raw, o.Cluster.ControlPlane.ExternalEndpoints...)
	if o.JoinSecret != nil {
		raw = append(raw, o.JoinSecret.DiscoveryEndpoints...)
	}
	raw = append(raw, discovery.MasterEndpoints(o.Cluster)...)
	seen := map[string]bool{}
	out := make([]string, 0, len(raw))
	for _, ep := range raw {
		ep = strings.TrimSpace(ep)
		if ep == "" {
			continue
		}
		key := joinEndpointKey(ep)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, ep)
	}
	return out
}

// joinEndpointKey 把端点规范成 host:port，用于去重（不改变最终探测地址）。
func joinEndpointKey(endpoint string) string {
	e := strings.TrimSuffix(strings.TrimSpace(endpoint), "/")
	e = strings.TrimPrefix(e, "https://")
	e = strings.TrimPrefix(e, "http://")
	if host, port, err := net.SplitHostPort(e); err == nil && host != "" && port != "" {
		return strings.ToLower(host) + ":" + port
	}
	return strings.ToLower(e) + ":6443"
}

// joinCACertHashes 返回 join 凭证里的 CA pin（--discovery-token-ca-cert-hash）。
// discoveryOptions 统一三处 discovery 构造，避免字段漂移：
// 端点、token、CA pin 与期望 clusterID 必须来自同一份 join/集群配置。
func (o *Options) discoveryOptions(token bootstrap.Token, endpoints []string, timeout time.Duration) discovery.Options {
	return discovery.Options{
		Endpoints:         endpoints,
		Token:             token,
		CACertHashes:      o.joinCACertHashes(),
		ExpectedClusterID: o.ClusterID,
		Timeout:           timeout,
	}
}

func (o *Options) joinCACertHashes() []string {
	if o.JoinSecret == nil {
		return nil
	}
	return o.JoinSecret.CACertHashes
}
