// Package kubeconfig 实现 `kubeconfig` phase。
// 核心约定：master 本机组件访问 https://127.0.0.1:6443。worker 默认通过
// 本地代理访问；设置 externalEndpoints 时改用该入口，否则回退到第一个 master。
// master 扩容不需要修改 master 本机的 kubeconfig。
// 角色差异：
// scheduler.conf、kubelet.conf —— 全部由本机 CA 直接签发；
// - worker：bootstrap-kubelet.conf —— 用 join 凭证里的引导令牌，
// CA 来自 cluster-info（由 discovery 写入 pki/ca.crt）；
// 正式 kubelet.conf 由 kubelet 自己完成 TLS Bootstrap 后轮换产生。
package kubeconfig

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"kubemono/internal/constants"
	"kubemono/internal/kubeconfig"
	"kubemono/internal/phase"
	"kubemono/internal/pki"
	"kubemono/internal/util/fsutil"
	"kubemono/internal/util/stringsx"
)

// PhaseName 是 `kubeconfig` phase 的名称。
const PhaseName = "kubeconfig"

func init() { phase.MustRegister(&Phase{}) }

// Phase 写入本节点的 kubeconfig 文件。
type Phase struct {
}

// Name 实现 phase.Phase。
func (p *Phase) Name() string { return PhaseName }

// Applies 报告本节点是否需要 kubeconfig 文件：所有节点都需要。
func (p *Phase) Applies(c *phase.Context) bool {
	_, ok := c.Self()
	return ok
}

// identity 描述 kubemono 必须落地的一份凭据。
type identity struct {
	// user 是 kubeconfig 用户条目名，也是证书 CN。
	user string
	// groups 写入证书的 O。
	groups []string
	// conf 是 /etc/kubernetes 内的目标 kubeconfig 文件名。
	conf string
	// certFile/keyFile 是客户端证书路径（位于 PKI 目录内）。
	certFile string
	keyFile  string
	// tokenOnly 标记使用引导令牌而非证书的身份。
	tokenOnly bool
	// server 是该 kubeconfig 指向的 apiserver 地址。
	server func(c *phase.Context) string
}

// serverForBootstrap 返回**加入节点做 TLS Bootstrap 时真正可达**的 apiserver 地址。
// worker 本机组件平时访问 https://127.0.0.1:6443（本地代理），但引导阶段本地代理
// 还不存在——它本身是 kubelet 管的静态 Pod（haproxy）。用 127.0.0.1 引导会死锁：
// 真机实测 worker 的 kubelet 报 "No valid client certificate is found but the
// server is not responsive"，5 分钟也拿不到证书，节点永远不 Ready。
// 因此引导文件指向可直连的入口：externalEndpoints 或第一个 master，绝不指向
// 尚未启动的 127.0.0.1 本地代理；external LB 地址也不用于引导（它可能只在
// 集群外可达）。
func serverForBootstrap(c *phase.Context) string {
	return clusterEndpoint(c)
}

// clusterEndpoint 返回本地代理禁用时使用的固定可达 apiserver 端点：依次为
// external LB/externalEndpoints、第一个 master 地址。
func clusterEndpoint(c *phase.Context) string {
	if c != nil && c.Cluster != nil {
		if ep, ok := c.Cluster.ExternalEndpointsAPIEndpoint(constants.APIServerPort); ok {
			return ep
		}
		if ep, ok := c.Cluster.FirstMasterAPIEndpoint(constants.APIServerPort); ok {
			return ep
		}
	}
	return serverForLocal(c)
}

// serverForLocal 返回节点本地的 apiserver 地址。
func serverForLocal(*phase.Context) string {
	return fmt.Sprintf("https://127.0.0.1:%d", constants.DefaultLocalProxyPort)
}

// desiredIdentities 按稳定顺序列出本节点必须拥有的凭据。
func desiredIdentities(c *phase.Context) ([]identity, error) {
	host, ok := c.Self()
	if !ok {
		return nil, fmt.Errorf("无法确定本机在集群配置中的条目")
	}
	target := pki.Target{
		IsMaster: host.IsMaster(),
		IsWorker: host.IsWorker(),
	}
	var out []identity
	for _, leaf := range pki.DefaultCatalogue.KubeconfigLeaves() {
		if !leaf.Applies(target) {
			continue
		}
		user, groups := leaf.SubjectFor(c.NodeName())
		out = append(out, identity{
			user:     user,
			groups:   groups,
			conf:     leaf.KubeconfigFile,
			certFile: leaf.CertFile,
			keyFile:  leaf.KeyFile,
			server:   serverForLocal,
		})
	}
	return out, nil
}

// bootstrapIdentity 列出加入中的 worker 基于令牌的 kubeconfig。
func bootstrapIdentity(c *phase.Context) (identity, bool) {
	host, ok := c.Self()
	if !ok || host.IsMaster() {
		return identity{}, false
	}
	return identity{
		user:      constants.UserKubeletBootstrap,
		conf:      constants.BootstrapKubeletKubeconfigName,
		tokenOnly: true,
		// 引导阶段本地代理还没起来，必须直连 master（见 serverForBootstrap）。
		server: serverForBootstrap,
	}, true
}

// Check 报告每个必需的 kubeconfig 是否已存在且携带预期的身份。
// 只判断“文件存在”是不够的：从别的节点拷贝过来的 kubeconfig 会让组件以错误的
// 身份连上 apiserver（例如 worker 拿着 master 的 kubelet 凭据）。因此这里还会
// 解析文件，确认当前上下文的用户名与预期一致。
func (p *Phase) Check(_ context.Context, c *phase.Context) (bool, error) {
	host, ok := c.Self()
	if !ok {
		return false, fmt.Errorf("无法确定本机在集群配置中的条目")
	}
	if !host.IsMaster() {
		// worker 完成 TLS Bootstrap 后 kubelet.conf 由 kubelet 自己写入、
		// bootstrap 文件被删除，两者之一属于本节点即视为完成。
		kubeletConf := c.Paths.KubeconfigPath(constants.KubeletKubeconfigName)
		if user, _, err := kubeconfigIdentity(kubeletConf); err == nil && user == "system:node:"+c.NodeName() {
			return true, nil
		}
		bootstrapConf := c.Paths.KubeconfigPath(constants.BootstrapKubeletKubeconfigName)
		if user, server, err := kubeconfigIdentity(bootstrapConf); err == nil &&
			user == constants.UserKubeletBootstrap && server == serverForBootstrap(c) {
			return true, nil
		}
		return false, nil
	}

	ids, err := desiredIdentities(c)
	if err != nil {
		return false, err
	}
	for _, id := range ids {
		expected := id.user
		if id.conf == constants.KubeletKubeconfigName {
			expected = "system:node:" + c.NodeName()
		}
		user, server, err := kubeconfigIdentity(c.Paths.KubeconfigPath(id.conf))
		if err != nil || user != expected {
			return false, nil
		}
		if id.server != nil {
			if want := id.server(c); server != want {
				return false, nil
			}
		}
	}
	if !userKubeconfigReady(c) {
		return false, nil
	}
	return true, nil
}

// isManagedAdminKubeconfig 报告路径是否是 kubemono 管理的 admin.conf。
func isManagedAdminKubeconfig(path string) bool {
	return filepath.Base(path) == constants.AdminKubeconfigName &&
		strings.Contains(filepath.ToSlash(path), "/etc/kubernetes/")
}

// userKubeconfigReady 报告 master 的 ~/.kube/config 是否已可用。
// 用户已有自己的 kubeconfig 时视为完成（不会被覆盖）；只有完全缺失时才需要
// Run 创建 admin.conf 的软链。
func userKubeconfigReady(c *phase.Context) bool {
	admin := c.Paths.KubeconfigPath(constants.AdminKubeconfigName)
	if !fsutil.FileExists(admin) {
		return true
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return true
	}
	link := filepath.Join(home, ".kube", "config")
	if target, err := os.Readlink(link); err == nil {
		return target == admin || !isManagedAdminKubeconfig(target)
	}
	if _, err := os.Lstat(link); err == nil {
		return true
	}
	return false
}

// kubeconfigIdentity 返回当前上下文的用户与 server。
func kubeconfigIdentity(path string) (string, string, error) {
	cfg, err := kubeconfig.Load(path)
	if err != nil {
		return "", "", err
	}
	ctx, ok := cfg.Contexts[cfg.CurrentContext]
	if !ok || ctx == nil {
		return "", "", fmt.Errorf("%s: 缺少当前上下文", path)
	}
	cluster, ok := cfg.Clusters[ctx.Cluster]
	if !ok || cluster == nil {
		return "", "", fmt.Errorf("%s: 缺少集群 %q", path, ctx.Cluster)
	}
	return ctx.AuthInfo, cluster.Server, nil
}

// Run 签发客户端证书并写入所有 kubeconfig。
func (p *Phase) Run(_ context.Context, c *phase.Context) error {
	report := c.Reporter()
	ids, err := desiredIdentities(c)
	if err != nil {
		return err
	}
	if boot, ok := bootstrapIdentity(c); ok {
		ids = append(ids, boot)
	}

	// 需要签发证书的身份必须先有 CA；只有引导凭据（token）的节点不需要。
	if needsCA(ids) {
		if !fsutil.FileExists(c.Paths.CertPath(constants.CACertName)) {
			return fmt.Errorf("缺少 CA 证书 %s：请先执行 `certs` phase（worker 的 CA 由集群发现阶段写入）",
				c.Paths.CertPath(constants.CACertName))
		}
	}
	var ca *pki.CA
	if needsCA(ids) {
		ca, err = pki.LoadCA(c.Paths.CertPath(constants.CACertName), c.Paths.CertPath(constants.CAKeyName))
		if err != nil {
			return fmt.Errorf("读取 CA 失败（master 才能签发本机凭据）: %w", err)
		}
	}

	validity := c.Cluster.CertValidity()
	if validity <= 0 {
		validity = constants.DefaultCertTTL
	}

	for _, id := range ids {
		confPath := c.Paths.KubeconfigPath(id.conf)
		cfg := kubeconfig.Config{
			User:   id.user,
			Server: id.server(c),
		}
		switch {
		case id.tokenOnly:
			if c.JoinSecret == nil || c.JoinSecret.Token == "" {
				return fmt.Errorf("生成 %s 需要 bootstrap token：请用 --token 传入", id.conf)
			}
			cfg.Token = c.JoinSecret.Token
			cfg.CACertPath = c.Paths.CertPath(constants.CACertName)
		default:
			certPath := c.Paths.CertPath(id.certFile)
			keyPath := c.Paths.CertPath(id.keyFile)
			created, err := ensureClientCert(c, ca, id, certPath, keyPath, validity)
			if err != nil {
				return err
			}
			if created {
				report.Infof("已签发客户端证书 %s（CN=%s）", certPath, id.user)
			}
			cfg.CACertPath = c.Paths.CertPath(constants.CACertName)
			cfg.ClientCertPath = certPath
			cfg.ClientKeyPath = keyPath
		}

		if c.DryRun {
			report.Infof("将写入 %s（user=%s server=%s）", confPath, cfg.User, cfg.Server)
			continue
		}
		if err := kubeconfig.WriteToFile(confPath, kubeconfig.Build(cfg)); err != nil {
			return err
		}
		report.Infof("已写入 %s", confPath)
	}
	if host, ok := c.Self(); ok && host.IsMaster() {
		if err := installUserKubeconfig(c, report); err != nil {
			return err
		}
	}
	return nil
}

// installUserKubeconfig 在 master 的 ~/.kube/config 建立指向 admin.conf 的软链。
// 这样 master 上直接执行 kubectl 就使用管理员凭据，不需要每次写 --kubeconfig。
// 只创建软链，不覆盖用户已有的 kubeconfig；adminKubeconfig=false 时跳过。
func installUserKubeconfig(c *phase.Context, report interface {
	Infof(string, ...any)
	Warnf(string, ...any)
}) error {
	admin := c.Paths.KubeconfigPath(constants.AdminKubeconfigName)
	if !fsutil.FileExists(admin) {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		report.Warnf("无法确定 HOME，跳过 ~/.kube/config 软链（admin.conf 仍可用）")
		return nil
	}
	dir := filepath.Join(home, ".kube")
	link := filepath.Join(dir, "config")
	if target, err := os.Readlink(link); err == nil {
		if target == admin {
			return nil
		}
		if !isManagedAdminKubeconfig(target) {
			report.Warnf("%s 已存在且指向 %s，未覆盖；如需使用 admin.conf 请手工调整", link, target)
			return nil
		}
		// 已有的 admin.conf 软链：替换为当前集群的 admin.conf。
		if err := os.Remove(link); err != nil {
			return fmt.Errorf("替换 %s 失败: %w", link, err)
		}
	}
	if _, err := os.Lstat(link); err == nil {
		report.Warnf("%s 已存在，未覆盖；如需使用 admin.conf 请手工调整", link)
		return nil
	}
	if c.DryRun {
		report.Infof("将创建 %s -> %s", link, admin)
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("创建 %s 失败: %w", dir, err)
	}
	if err := os.Symlink(admin, link); err != nil {
		return fmt.Errorf("创建 %s 失败: %w", link, err)
	}
	report.Infof("已创建 %s -> %s（kubectl 可直接使用）", link, admin)
	return nil
}

// ensureClientCert 创建或复用客户端证书。
// 复用已存在的证书是关键：重复执行 install 不能改变任何 kubeconfig 指向的
// 凭据，否则已经运行的组件会在下一次重启后以另一个身份出现。
// 但复用必须校验“身份 + 签发者”：只按文件存在/未到期复用，会在 CA 被替换或
// 使用另一套集群材料时留下旧 CA 签发的证书，apiserver 直接拒绝。
func ensureClientCert(c *phase.Context, ca *pki.CA, id identity, certPath, keyPath string, validity time.Duration) (bool, error) {
	if pki.CertificateExists(certPath, keyPath) && clientCertMatches(certPath, ca, id) {
		return false, nil
	}
	if c.DryRun {
		return true, nil
	}
	cert, key, err := ca.Sign(pki.CertConfig{
		CommonName:    id.user,
		Organizations: id.groups,
		Usages:        pki.UsageClient,
		Validity:      validity,
	})
	if err != nil {
		return false, fmt.Errorf("签发 %s 的客户端证书: %w", id.user, err)
	}
	if err := pki.WriteCertAndKey(certPath, keyPath, cert, key); err != nil {
		return false, err
	}
	return true, nil
}

// clientCertMatches 报告现有证书是否具有预期身份，并由当前 CA 签发。
func clientCertMatches(certPath string, ca *pki.CA, id identity) bool {
	if ca == nil || ca.Cert == nil {
		return false
	}
	cert, err := pki.LoadCertificate(certPath)
	if err != nil {
		return false
	}
	if cert.Subject.CommonName != id.user {
		return false
	}
	if !stringsx.SetEqual(cert.Subject.Organization, id.groups) {
		return false
	}
	return cert.CheckSignatureFrom(ca.Cert) == nil
}

// needsCA 报告是否有任何身份需要客户端证书。
func needsCA(ids []identity) bool {
	for _, id := range ids {
		if !id.tokenOnly {
			return true
		}
	}
	return false
}
