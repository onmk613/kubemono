// Package discovery 实现加入节点侧的集群发现。
// 安全模型：
// 1. 用引导令牌向任一 master 读取 kube-public/cluster-info，**TLS 不校验**——
// 此时还不知道该信任谁；
// 2. 用令牌的 secret 校验 cluster-info 的 JWS 签名，签名通过才信任其中的 CA；
// 3. join 凭证里可选的 caCertHashes 提供第二次锁定（CA 轮换场景下的防降级）。
// 因此"能连上 6443"不等于"可信"：没有有效 JWS 的响应会被直接丢弃。
package discovery

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"kubemono/internal/bootstrap"
	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
)

// kubemono 在 upstream kubeconfig 键之外使用的 ConfigMap data 键。
const (
	// ClusterIDKey 承载 cluster-info 中不可变的集群身份。
	ClusterIDKey = "kubemono-cluster-id"
	// ClusterIDJWSPrefix 是集群身份按令牌区分的 JWS 键。
	ClusterIDJWSPrefix = "jws-cluster-id-"
	// ReleaseKey 承载创建该集群的 kubemono release（已知时）。
	ReleaseKey = "kubemono-release"
)

// ConfigMapReader 从单个端点读取 cluster-info ConfigMap。
type ConfigMapReader interface {
	// ClusterInfo 从给定的 server URL 读取 kube-public/cluster-info。
	ClusterInfo(ctx context.Context, server string) (*corev1.ConfigMap, error)
}

// ErrUnreachable 表示所有端点都在传输层失败（连接被拒绝、DNS、超时）。
// 它刻意区别于认证失败或 JWS 失败：前者是"集群还没起来"，可以等待；后者绝
// 不能被重试，也不能当作"没有集群"。
var ErrUnreachable = errors.New("discovery: 所有发现端点均不可达")

// Info 是已发现集群经过校验的描述。
type Info struct {
	// CA 是令牌 JWS 作保的集群 CA（PEM 编码）。
	CA []byte
	// ClusterID 是集群身份；集群早于 kubemono 的 cluster-id 键时为空
	// （视为"未知，必须由配置确认"）。
	ClusterID string
	// Endpoint 是做出应答的 master 地址。
	Endpoint string
}

// Options 配置一次探测。
type Options struct {
	// Endpoints 是要依次尝试的 master 地址，每个都会被转成
	Endpoints []string
	// Port 默认为 constants.APIServerPort。
	Port int
	// Token 认证 cluster-info 的读取。
	Token bootstrap.Token
	// CACertHashes 可选地锁定 CA 公钥。
	CACertHashes []string
	// Reader 执行读取；为 nil 时使用 client-go 实现。
	Reader ConfigMapReader
	// Timeout 限制单次端点尝试的时长。
	Timeout time.Duration
	// ClusterID 可选地断言期望的集群身份。
	ExpectedClusterID string
}

// Probe 依次尝试每个端点，直到某个端点给出可校验的 cluster-info。
// 返回 (nil, nil) 表示"没有任何 endpoint 给出可信的响应"（未找到集群），
// 而不是错误：调用方据此走 init 或等待逻辑。
func Probe(ctx context.Context, opts Options) (*Info, error) {
	if len(opts.Token.ID) == 0 || len(opts.Token.Secret) == 0 {
		return nil, fmt.Errorf("discovery: 需要引导令牌才能校验 cluster-info 的签名")
	}
	reader := opts.Reader
	if reader == nil {
		// 必须把引导令牌交给 reader：cluster-info 要认证，匿名读会被 RBAC 拒绝。
		reader = NewClientReader(opts.Timeout, opts.Token.String())
	}
	port := opts.Port
	if port == 0 {
		port = constants.APIServerPort
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}

	// 收集**每个**端点的失败原因：只报最后一个端点会把真正的根因埋掉。
	// 真机实测：HA 集群里 m2 加入时明明能连上 m1（校验失败），却只报
	// "从 https://10.8.2.103:6443 ... connection refused"（103 还没装），
	// 排障时被彻底误导。
	var failures []string
	hardFailures := 0
	for _, endpoint := range opts.Endpoints {
		server := endpointURL(endpoint, port)
		attemptCtx, cancel := context.WithTimeout(ctx, timeout)
		cm, err := reader.ClusterInfo(attemptCtx, server)
		cancel()
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", server, err))
			if !IsUnreachable(err) {
				hardFailures++
			}
			continue
		}
		info, err := verify(cm, opts, server)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", server, err))
			// JWS/身份/CA 校验失败是确定性错误，绝不能当成“集群还没起来”。
			hardFailures++
			continue
		}
		return info, nil
	}
	if len(failures) > 0 {
		if hardFailures == 0 {
			return nil, fmt.Errorf("%w：\n  %s", ErrUnreachable, strings.Join(failures, "\n  "))
		}
		// 至少一个端点是“可信失败”（签名/身份/配置错误）：返回硬错误，
		// 让调用方停止等待并暴露真正的根因。
		return nil, fmt.Errorf("所有发现端点都失败：\n  %s", strings.Join(failures, "\n  "))
	}
	return nil, nil
}

// IsUnreachable 判断错误是否为传输层失败。只有这类失败可以在等待 init 节点
// 期间安全重试；校验类错误必须快速失败。
func IsUnreachable(err error) bool {
	if err == nil {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, s := range []string{
		"connection refused", "no such host", "i/o timeout", "connection reset",
		"network is unreachable", "no route to host", "context deadline exceeded",
		"tls handshake timeout",
	} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// verify 校验 JWS 签名并提取可信的集群数据。
func verify(cm *corev1.ConfigMap, opts Options, endpoint string) (*Info, error) {
	if cm == nil {
		return nil, fmt.Errorf("discovery: %s 返回了空的 cluster-info", endpoint)
	}
	// ① kubeconfig blob 的 JWS（上游约定）：签名通过才信任其中的 CA。
	ci, err := bootstrap.ParseClusterInfoConfigMap(cm, opts.Token)
	if err != nil {
		return nil, err
	}
	// ② CA 二次锁定（join 凭证里的 caCertHashes）。
	if err := bootstrap.VerifyCACertHashes(ci.CA, opts.CACertHashes); err != nil {
		return nil, err
	}
	info := &Info{CA: ci.CA, Endpoint: endpoint}

	// ③ 集群身份：必须同样带 JWS，否则无法防伪造。
	rawID, ok := cm.Data[ClusterIDKey]
	if ok && rawID != "" {
		sig, ok := cm.Data[ClusterIDJWSPrefix+opts.Token.ID]
		if !ok || sig == "" {
			return nil, fmt.Errorf("discovery: cluster-info 里的集群身份缺少签名，拒绝信任")
		}
		if !bootstrap.VerifyDetachedSignature(sig, rawID, opts.Token) {
			return nil, fmt.Errorf("discovery: 集群身份的签名校验失败（可能是被篡改的 cluster-info）")
		}
		info.ClusterID = strings.TrimSpace(rawID)
		if opts.ExpectedClusterID != "" && info.ClusterID != opts.ExpectedClusterID {
			return nil, fmt.Errorf("discovery: 探测到的集群 clusterID=%s 与期望的 %s 不一致，拒绝加入",
				info.ClusterID, opts.ExpectedClusterID)
		}
	} else if opts.ExpectedClusterID != "" {
		// 目标集群没有公布身份（旧集群）：不能确认它就是要加入的集群。
		return nil, fmt.Errorf("discovery: 目标集群没有公布 clusterID，无法确认身份（期望 %s）", opts.ExpectedClusterID)
	}
	return info, nil
}

// endpointURL 把配置里的地址转成 https URL。
func endpointURL(endpoint string, port int) string {
	e := strings.TrimSpace(strings.TrimSuffix(endpoint, "/"))
	if strings.HasPrefix(e, "https://") || strings.HasPrefix(e, "http://") {
		return e
	}
	if _, _, err := splitHostPort(e); err == nil {
		return "https://" + e
	}
	return fmt.Sprintf("https://%s:%d", e, port)
}

// splitHostPort 是一个独立的小工具，让 endpointURL 保持可读。
func splitHostPort(s string) (string, string, error) {
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return s, "", fmt.Errorf("no port")
	}
	return s[:i], s[i+1:], nil
}

// MasterEndpoints 按配置顺序返回要探测的地址。
// 使用 AllAddresses：matchAddresses 是 NAT 场景下的备用可达地址。
func MasterEndpoints(c *clusterconfig.Cluster) []string {
	if c == nil {
		return nil
	}
	var out []string
	for _, m := range c.Cluster.Hosts.Masters() {
		out = append(out, m.AllAddresses()...)
	}
	return out
}
