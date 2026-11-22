// Package certs 实现 `certs` phase：本机 PKI 的生成与续期
// 规则：
// - 叶子证书由本机签发，私钥不离开节点；
// - master 上才持有 CA 私钥；worker 的 ca.crt 由加入流程（discovery）写入，
// 因此本 phase 在 worker 上不适用；
// - 证书是“生成一次、按需续期”的产物，不是渲染结果，所以直接写盘、
// 不进 desired；到期检查由 `kubemono cert check-expiration` 负责。
package certs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	"kubemono/internal/controlplane"
	"kubemono/internal/phase"
	"kubemono/internal/pki"
	"kubemono/internal/systemd"
	"kubemono/internal/util/fsutil"
)

// PhaseName 是 `certs` phase 的名称。
const PhaseName = "certs"

func init() { phase.MustRegister(&Phase{}) }

// certRestartMarker 记录“证书已重签但组件尚未成功重启”。
// 如果没有它，重启失败或进程在写证书与重启之间被杀后，下一次 certs.Check
// 会因为证书已经匹配新 SAN 而返回 true，永远不重试重启。
const certRestartMarker = "cert-restart.json"

type certRestartState struct {
	Components []string `json:"components"`
}

func certRestartPath(c *phase.Context) string {
	return filepath.Join(c.Paths.Kubemono(), certRestartMarker)
}

func readPendingCertRestart(c *phase.Context) []string {
	data, err := os.ReadFile(certRestartPath(c))
	if err != nil {
		return nil
	}
	var st certRestartState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil
	}
	return st.Components
}

func writePendingCertRestart(c *phase.Context, components map[string]bool) error {
	if len(components) == 0 {
		return nil
	}
	names := make([]string, 0, len(components))
	for name := range components {
		names = append(names, name)
	}
	sort.Strings(names)
	data, err := json.Marshal(certRestartState{Components: names})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(certRestartPath(c)), 0o700); err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(certRestartPath(c), append(data, '\n'), 0o600)
}

func clearPendingCertRestart(c *phase.Context) error {
	return fsutil.RemoveIfExists(certRestartPath(c))
}

// Restarter 重启因证书重签而受影响的组件。
// 测试注入它即可避免真实 systemctl；生产实现只重启本机已存在的 unit。
type Restarter interface {
	Restart(ctx context.Context, components []string) error
}

// Phase 生成本机 PKI。
type Phase struct {
	// Restarter 覆盖重签后的组件重启；nil 时使用本机 systemd。
	Restarter Restarter
}

// Name 实现 phase.Phase。
func (p *Phase) Name() string { return PhaseName }

// Applies 表示本节点是否持有集群 CA。
// master 和 etcd 节点都需要 CA 私钥；worker 只有 ca.crt（由加入流程下发）。
func (p *Phase) Applies(c *phase.Context) bool {
	return c.Cluster != nil && c.IsMaster()
}

// desiredCert 是一张待签发/校验的叶子证书。
type desiredCert struct {
	leaf pki.Leaf
	cfg  pki.CertConfig
}

// desiredTargets 计算本节点必须具备的每一张证书。
// 纯函数：不读盘、不访问集群，因此单元测试可以直接驱动它。
func desiredTargets(c *phase.Context) ([]desiredCert, error) {
	host, ok := c.Self()
	if !ok {
		return nil, fmt.Errorf("无法确定本机在集群配置中的条目")
	}
	spec := c.Cluster
	validity := spec.CertValidity()
	if validity <= 0 {
		validity = constants.DefaultCertTTL
	}
	// 保留原有校验：非法 keyAlgorithm 即使不参与本 phase 的签发也必须报错。
	if _, err := toKeyAlgorithm(spec.ControlPlane.PKI.KeyAlgorithm); err != nil {
		return nil, err
	}
	target := pki.Target{
		IsMaster: host.IsMaster(),
		IsWorker: host.IsWorker(),
		RunsEtcd: runsEtcd(c),
	}
	var out []desiredCert
	for _, leaf := range pki.DefaultCatalogue.CertsLeaves(target) {
		cfg := leaf.CertConfig(c.NodeName(), validity)
		switch leaf.ID {
		case pki.CertAPIServer:
			cfg.DNSNames, cfg.IPAddresses = apiServerSANs(c)
		case pki.CertEtcdServer, pki.CertEtcdPeer:
			cfg.DNSNames = []string{c.NodeName(), "localhost"}
			cfg.IPAddresses = ips(append(host.AllAddresses(), "127.0.0.1"))
		}
		out = append(out, desiredCert{leaf: leaf, cfg: cfg})
	}
	return out, nil
}

// apiServerSANs 计算 apiserver 服务端证书的 SAN。
//
// 只断言"本节点真正能服务的名字"：
//   - 公共：kubernetes 的四个 Service 名、Service IP、127.0.0.1、localhost，
//     以及 externalEndpoints / certSANs（VIP、DNS、LB 这类集群共享入口）；
//   - 本机：Host.Name、NodeName、Host.AllAddresses()。
//
// 不把其它 master 的名字/地址塞进每台证书：证书只应覆盖持有者实际服务的
// 端点，m2 的私钥不该顺带冒充 m1。共享入口请显式写 externalEndpoints/certSANs。
// 附带的好处是新增/删除 master 不改变存量 master 的 desired SAN——否则每次
// 扩容都会让所有 master 的 certs phase 判成漂移、重签并重启 apiserver。
func apiServerSANs(c *phase.Context) ([]string, []net.IP) {
	host, _ := c.Self()
	spec := c.Cluster
	apiDNS := []string{
		"kubernetes", "kubernetes.default", "kubernetes.default.svc",
		"kubernetes.default.svc." + spec.Network.DNSDomain,
	}
	var apiIPs []string
	if ip, err := clusterconfig.FirstUsableServiceIP(spec.Network.ServiceCIDR, 1); err == nil {
		apiIPs = append(apiIPs, ip)
	}
	apiIPs = append(apiIPs, "127.0.0.1")
	if host != nil {
		apiDNS = append(apiDNS, host.Name)
		apiIPs = append(apiIPs, host.AllAddresses()...)
	}
	if nodeName := c.NodeName(); nodeName != "" && (host == nil || nodeName != host.Name) {
		apiDNS = append(apiDNS, nodeName)
	}
	for _, ep := range spec.ControlPlane.ExternalEndpoints {
		dns, ip := splitEndpoint(ep)
		if ip != nil {
			apiIPs = append(apiIPs, ip.String())
			continue
		}
		apiDNS = append(apiDNS, dns)
	}
	for _, san := range spec.ControlPlane.CertSANs {
		if ip := net.ParseIP(strings.TrimSpace(san)); ip != nil {
			apiIPs = append(apiIPs, ip.String())
			continue
		}
		apiDNS = append(apiDNS, san)
	}
	apiDNS = append(apiDNS, "localhost")
	return apiDNS, ips(apiIPs)
}

// runsEtcd 表示 etcd 是否运行在本节点。
func runsEtcd(c *phase.Context) bool {
	host, ok := c.Self()
	if !ok {
		return false
	}
	return !c.Cluster.ControlPlane.Etcd.IsExternal() && host.IsMaster()
}

// Check 表示每张证书是否都已存在且可用。
func (p *Phase) Check(_ context.Context, c *phase.Context) (bool, error) {
	if len(readPendingCertRestart(c)) > 0 {
		// 证书可能已经写好，但上次重启失败：必须让 Run 继续完成重启。
		return false, nil
	}
	targets, err := desiredTargets(c)
	if err != nil {
		return false, err
	}
	for _, t := range targets {
		certPath, keyPath := t.leaf.Paths(c.Paths)
		if !pki.CertificateExists(certPath, keyPath) {
			return false, nil
		}
		// 读取/解析失败必须上抛而不是当作"未完成"：本地文件损坏不会自愈，
		// 吞掉会让每次 Check 都重签一遍（Run 里会拒绝重签 CA，于是永远卡住）。
		matches, err := pki.CertificateMatchesConfig(certPath, t.cfg)
		if err != nil {
			return false, fmt.Errorf("certs: 检查 %s 失败: %w", certPath, err)
		}
		if !matches {
			return false, nil
		}
	}
	// CA 与 ServiceAccount 密钥同样必须在位，否则控制面起不来。
	// CA 用 CAExists：它不套用叶子证书的 30 天续期窗口，否则 CA 临近到期时
	// Check 会返回 false，Run 进而重建 CA、覆盖现有信任根。
	for _, pair := range caPairs(c) {
		certPath, keyPath := pair.Paths(c.Paths)
		external, externalConfigured, err := externalCA(c, pair.ID)
		if err != nil {
			return false, err
		}
		if externalConfigured {
			ca, present, loadErr := pki.LoadCAIfExists(certPath, keyPath)
			if loadErr != nil {
				return false, loadErr
			}
			if !present || !bytes.Equal(ca.Cert.Raw, external.Cert.Raw) {
				return false, nil
			}
			continue
		}
		if !pki.CAExists(certPath, keyPath) {
			return false, nil
		}
	}
	saKey := c.Paths.ServiceAccountKey()
	saPub := c.Paths.ServiceAccountPub()
	if !pki.FileExists(saKey) || !pki.FileExists(saPub) {
		// 首次 init 由 Run 生成；已安装节点缺失时 Run 会 fail closed。
		return false, nil
	}
	if _, err := pki.LoadServiceAccountKeyPair(saPub, saKey); err != nil {
		return false, fmt.Errorf("ServiceAccount 密钥对不可用（%s / %s）：%w；"+
			"已安装节点不会自动重建 SA 密钥，请从证书包或备份恢复", saPub, saKey, err)
	}
	return true, nil
}

// Run 创建各个 CA、ServiceAccount 密钥对以及每一张叶子证书。
func (p *Phase) Run(ctx context.Context, c *phase.Context) error {
	report := c.Reporter()
	spec := c.Cluster

	// 1. CA 与 SA 密钥（幂等：已存在且可用就复用，绝不重签 CA）。
	kas := map[pki.CAID]*pki.CA{}
	for _, pair := range caPairs(c) {
		ca, err := loadOrCreateCA(pair, spec, c)
		if err != nil {
			return err
		}
		kas[pair.ID] = ca
		certPath, _ := pair.Paths(c.Paths)
		report.Debugf("CA %s 就绪（%s）", pair.CN(), certPath)
	}
	saKey := c.Paths.ServiceAccountKey()
	saPub := c.Paths.ServiceAccountPub()
	if _, err := pki.LoadServiceAccountKeyPair(saPub, saKey); err != nil {
		keyExists := pki.FileExists(saKey)
		pubExists := pki.FileExists(saPub)
		switch {
		case c.DryRun:
			report.Infof("将生成 ServiceAccount 密钥对（dry-run）")
		case keyExists || pubExists:
			// 只缺一个或内容不匹配：可能是密钥被替换/损坏。重建会让本机与其他
			// master 的 SA 公钥分叉，已有 token 也无法验证，必须人工恢复。
			return fmt.Errorf("ServiceAccount 密钥对不可用（key=%t，pub=%t）：%w；"+
				"为避免与其他 master 分叉，拒绝自动重建；请从证书包或备份恢复", keyExists, pubExists, err)
		case !c.IsInitNode:
			return fmt.Errorf("ServiceAccount 密钥不存在（key=%t，pub=%t）：已安装/加入节点不会自动生成 SA 密钥；"+
				"请确认 join 证书包已导入，或从备份恢复", keyExists, pubExists)
		default:
			if err := pki.GenerateServiceAccountKeyPair(saPub, saKey); err != nil {
				return fmt.Errorf("生成 ServiceAccount 密钥对: %w", err)
			}
		}
	}

	// 2. 叶子证书。
	targets, err := desiredTargets(c)
	if err != nil {
		return err
	}
	reissued := map[string]bool{}
	for _, component := range readPendingCertRestart(c) {
		reissued[component] = true
	}
	for _, t := range targets {
		certPath, keyPath := t.leaf.Paths(c.Paths)
		if pki.CertificateExists(certPath, keyPath) {
			// 证书“有效”不等于“仍是期望的身份”：SAN/CN/O/EKU 变化必须重签，
			// 否则新增 externalEndpoint/certSAN 后 apiserver 会继续出示旧证书。
			matches, matchErr := pki.CertificateMatchesConfig(certPath, t.cfg)
			if matchErr == nil && matches {
				// CertificateExists 已经判过续期窗口，这里不重复读盘校验到期。
				report.Debugf("证书 %s 已存在、有效且与期望 SAN 一致，跳过", certPath)
				continue
			}
		}
		if c.DryRun {
			report.Infof("将签发 %s（%s）", t.cfg.CommonName, certPath)
			continue
		}
		ca, ok := kas[t.leaf.CA]
		if !ok {
			return fmt.Errorf("签发 %s 需要 CA，但该 CA 在本节点不存在", t.cfg.CommonName)
		}
		// 在写证书之前落盘 marker：写成功但随后崩溃/重启失败时，下一次
		// Check 仍能发现“证书已换、组件未重启”。
		for _, component := range t.leaf.Restart {
			reissued[component] = true
		}
		if err := writePendingCertRestart(c, reissued); err != nil {
			return fmt.Errorf("记录证书重启待办失败: %w", err)
		}
		cert, key, err := ca.Sign(t.cfg)
		if err != nil {
			return fmt.Errorf("签发 %s: %w", t.cfg.CommonName, err)
		}
		if err := pki.WriteCertAndKey(certPath, keyPath, cert, key); err != nil {
			return err
		}
		report.Infof("已签发 %s", certPath)
	}
	if c.DryRun {
		return nil
	}
	return p.restartComponents(ctx, c, reissued)
}

// restartComponents 让已经运行的组件加载新证书。
// 组件 unit 不存在时跳过（首次 init 的 certs phase 早于 control-plane phase）；
// 只有本机确实在运行该组件时才需要重启。
func (p *Phase) restartComponents(ctx context.Context, c *phase.Context, components map[string]bool) error {
	if len(components) == 0 {
		return nil
	}
	names := make([]string, 0, len(components))
	for name := range components {
		names = append(names, name)
	}
	sort.Strings(names)
	if p.Restarter != nil {
		if err := p.Restarter.Restart(ctx, names); err != nil {
			return err
		}
		return clearPendingCertRestart(c)
	}
	var running []string
	for _, name := range names {
		unit := c.Paths.Resolve("/etc/systemd/system/" + name + ".service")
		if !fsutil.FileExists(unit) {
			c.Reporter().Debugf("证书已重签，但 %s 的 unit 尚不存在，跳过重启", name)
			continue
		}
		running = append(running, name)
	}
	if len(running) == 0 {
		return clearPendingCertRestart(c)
	}
	runner, err := controlplane.NewRunner(controlplane.Options{
		Paths:  c.Paths,
		Health: controlplane.NewHTTPChecker(),
		Report: func(format string, args ...any) { c.Reporter().Infof(format, args...) },
	}, systemd.Manager{Timeout: 30 * time.Second})
	if err != nil {
		return fmt.Errorf("构造证书重启执行器失败: %w", err)
	}
	for _, name := range running {
		if err := runner.Restart(ctx, controlplane.Component(name)); err != nil {
			return fmt.Errorf("证书重签后重启 %s 失败: %w", name, err)
		}
		c.Reporter().Infof("已重启 %s 以加载新证书", name)
	}
	return clearPendingCertRestart(c)
}

// caPairs 返回本节点必须持有的每个 CA。
func caPairs(c *phase.Context) []pki.Authority {
	var out []pki.Authority
	for _, a := range pki.DefaultCatalogue.Authorities {
		if a.ID == pki.CAEtcd && !runsEtcd(c) {
			continue
		}
		out = append(out, a)
	}
	return out
}

// externalCA 从 controlPlane.pki.externalCA 加载可选的企业 CA。
// 当前只把它用于集群根 CA（kubernetes-ca）。front-proxy-ca 与 etcd-ca 是独立
// 信任域，复用同一把企业 CA 会让三个 CA 的职责混在一起，因此不扩散。
func externalCA(c *phase.Context, id pki.CAID) (*pki.CA, bool, error) {
	if c == nil || c.Cluster == nil {
		return nil, false, nil
	}
	var ext clusterconfig.ExternalCASpec
	switch id {
	case pki.CAKubernetes:
		ext = c.Cluster.ControlPlane.PKI.ExternalCA
	case pki.CAFrontProxy:
		ext = c.Cluster.ControlPlane.PKI.ExternalFrontProxyCA
	case pki.CAEtcd:
		ext = c.Cluster.ControlPlane.PKI.ExternalEtcdCA
	default:
		return nil, false, nil
	}
	if ext.CertFile == "" && ext.KeyFile == "" {
		return nil, false, nil
	}
	if ext.CertFile == "" || ext.KeyFile == "" {
		return nil, true, fmt.Errorf("%s 的 certFile 与 keyFile 必须同时提供", externalCAField(id))
	}
	ca, err := pki.LoadCA(ext.CertFile, ext.KeyFile)
	if err != nil {
		return nil, true, fmt.Errorf("加载 %s（%s / %s）失败: %w", externalCAField(id), ext.CertFile, ext.KeyFile, err)
	}
	return ca, true, nil
}

// externalCAField 返回 CA 对应的配置字段名。
func externalCAField(id pki.CAID) string {
	switch id {
	case pki.CAKubernetes:
		return "controlPlane.pki.externalCA"
	case pki.CAFrontProxy:
		return "controlPlane.pki.externalFrontProxyCA"
	case pki.CAEtcd:
		return "controlPlane.pki.externalEtcdCA"
	default:
		return string(id)
	}
}

// loadOrCreateCA 加载已有的 CA，不存在则新建。
// 关键约束：CA 一旦存在就不允许因为“临近到期/无法解析”被自动重建。CA 轮换
// 会让所有既有叶子证书与 kubeconfig 失效，必须由显式运维流程完成。
func loadOrCreateCA(pair pki.Authority, spec *clusterconfig.Cluster, c *phase.Context) (*pki.CA, error) {
	certPath, keyPath := pair.Paths(c.Paths)
	ca, present, err := pki.LoadCAIfExists(certPath, keyPath)
	if err != nil {
		return nil, err
	}
	external, externalConfigured, err := externalCA(c, pair.ID)
	if err != nil {
		return nil, err
	}
	if externalConfigured {
		if present {
			if !bytes.Equal(ca.Cert.Raw, external.Cert.Raw) {
				return nil, fmt.Errorf("%s 与已有的集群 CA %s 不一致；"+
					"导入企业 CA 只能在创建集群时进行，已有 CA 不允许被覆盖", externalCAField(pair.ID), certPath)
			}
			c.Reporter().Infof("集群 CA 与企业 CA 一致，复用 %s", certPath)
			return ca, nil
		}
		if c.DryRun {
			c.Reporter().Infof("将导入企业 CA %s 到 %s（dry-run）", external.Cert.Subject.CommonName, certPath)
			return external, nil
		}
		if err := external.WriteToDisk(certPath, keyPath); err != nil {
			return nil, fmt.Errorf("导入企业 CA 到 %s 失败: %w", certPath, err)
		}
		c.Reporter().Infof("已导入企业 CA %s（%s）", external.Cert.Subject.CommonName, certPath)
		return external, nil
	}
	if present {
		return ca, nil
	}
	if c.DryRun {
		// dry-run 下不产生任何密钥材料：返回一个临时 CA 以便后续步骤继续模拟。
		ca, err := pki.NewCA(pair.CN(), pki.DefaultKeyAlgorithm, spec.CAValidity())
		if err != nil {
			return nil, err
		}
		c.Reporter().Infof("将生成 CA %s（dry-run）", pair.CN())
		return ca, nil
	}
	if !c.IsInitNode {
		// 已安装/正在 join 的节点不应凭空获得新的 CA：这会形成第二套信任根，
		// 既有叶子证书、kubeconfig 与 etcd 数据都可能因此失效。
		return nil, fmt.Errorf("CA %s 在本机不存在（%s 与 %s）：已安装或加入节点不会自动生成新 CA；"+
			"请从 PKI 备份恢复，或确认这是全新 init 后使用 kubemono init", pair.CN(), certPath, keyPath)
	}
	alg, err := toKeyAlgorithm(spec.ControlPlane.PKI.KeyAlgorithm)
	if err != nil {
		return nil, err
	}
	ca, err = pki.NewCA(pair.CN(), alg, spec.CAValidity())
	if err != nil {
		return nil, fmt.Errorf("生成 CA %s: %w", pair.CN(), err)
	}
	if err := ca.WriteToDisk(certPath, keyPath); err != nil {
		return nil, err
	}
	c.Reporter().Infof("已生成 CA %s", pair.CN())
	return ca, nil
}

// toKeyAlgorithm 把配置值映射到 internal/pki。
func toKeyAlgorithm(s string) (pki.KeyAlgorithm, error) {
	switch s {
	case "", clusterconfig.KeyAlgorithmRSA2048:
		return pki.RSA2048, nil
	case clusterconfig.KeyAlgorithmECDSAP256:
		return pki.ECDSAP256, nil
	default:
		return "", fmt.Errorf("不支持的密钥算法 %q（可选 %s、%s）", s, clusterconfig.KeyAlgorithmRSA2048, clusterconfig.KeyAlgorithmECDSAP256)
	}
}

// splitEndpoint 拆分 "host:port"（也可以是裸主机名），返回主机，并在主机是 IP 时
// 一并返回解析出的地址。
func splitEndpoint(endpoint string) (string, net.IP) {
	e := strings.TrimSpace(strings.TrimSuffix(endpoint, "/"))
	e = strings.TrimPrefix(e, "https://")
	e = strings.TrimPrefix(e, "http://")
	host := e
	if h, _, err := net.SplitHostPort(e); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if ip := net.ParseIP(host); ip != nil {
		return "", ip
	}
	return host, nil
}

// ips 解析地址，跳过所有不是 IP 的项。
func ips(addrs []string) []net.IP {
	seen := map[string]bool{}
	var out []net.IP
	for _, a := range addrs {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		// externalEndpoints 可能带端口。
		if host, _, err := net.SplitHostPort(a); err == nil {
			a = host
		}
		ip := net.ParseIP(a)
		if ip == nil || seen[ip.String()] {
			continue
		}
		seen[ip.String()] = true
		out = append(out, ip)
	}
	return out
}
