package certrenew

import (
	"context"
	"crypto/x509"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"kubemono/internal/constants"
	"kubemono/internal/pki"
)

// certNames 返回 catalogue 中的证书 ID 列表，便于错误提示。
func certNames() string {
	names := make([]string, 0, len(pki.DefaultCatalogue.AllLeaves()))
	for _, leaf := range pki.DefaultCatalogue.AllLeaves() {
		names = append(names, string(leaf.ID))
	}
	return strings.Join(names, ", ")
}

// caRef 是一个已加载的证书颁发机构。
type caRef struct {
	// Name 是 CA 名称（kubernetes-ca、front-proxy-ca、etcd-ca 等）。
	Name string
	// CertPath 是 CA 证书路径。
	CertPath string
	// CA 是已加载的颁发机构。
	CA *pki.CA
}

// fingerprintOfCertFile 读取证书文件并返回其中第一张证书的指纹。
// 读不到或解析失败时返回空串：调用方据此判断“CA 文件已经不可用”。
func fingerprintOfCertFile(path string) string {
	// pki 只提供原语；吞错返回空串是本包的选择（指纹基线缺失会让
	// 后续“CA 不能变”校验自然失败，不会静默放过）。
	sum, _ := pki.FingerprintOfPEMFile(path)
	return sum
}

// loadCAs 加载本机存在的所有 CA。
// worker 上没有 CA 私钥：它的叶子证书由 master 签发（TLS bootstrap 自动轮换），
// 因此 Local 在 worker 上会明确返回"本机没有 CA"的错误，而不是静默成功。
func loadCAs(paths constants.Paths) ([]caRef, error) {
	var out []caRef
	for _, a := range pki.DefaultCatalogue.Authorities {
		certPath, keyPath := a.Paths(paths)
		if !pki.FileExists(certPath) || !pki.FileExists(keyPath) {
			continue
		}
		ca, err := pki.LoadCA(certPath, keyPath)
		if err != nil {
			return nil, fmt.Errorf("读取 CA %s 失败: %w", certPath, err)
		}
		out = append(out, caRef{Name: a.CN(), CertPath: certPath, CA: ca})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("本机没有 CA 私钥（%s 不存在），无法续期；"+
			"worker 的客户端证书由 TLS bootstrap 自动轮换，服务端证书由 master 签发", paths.PKI())
	}
	return out, nil
}

// Local 用现有 CA 重签本机的叶子证书。
// 默认只重签已进入续期窗口（默认 30 天）或已过期的证书；Force 时全部重签。
// 重签保留原证书的主题、SAN 与扩展密钥用途，CA 与 ServiceAccount 密钥不变；
// 之后通过注入的 Restarter 重启受影响的组件。
func Local(ctx context.Context, opts Options) (*Report, error) {
	cas, err := loadCAs(opts.Paths)
	if err != nil {
		return nil, err
	}
	report := &Report{
		Node:           opts.nodeName(),
		CAFingerprints: map[string]string{},
		DryRun:         opts.DryRun,
	}
	// 指纹取自磁盘文件而不是内存里的 CA 对象：后续的“CA 不能变”校验必须能
	// 发现续期期间有人替换了 CA 文件，用内存对象自比永远相等。
	before := map[string]string{}
	for _, ca := range cas {
		before[ca.Name] = fingerprintOfCertFile(ca.CertPath)
		report.CAFingerprints[ca.Name] = before[ca.Name]
	}
	report.Notes = append(report.Notes,
		"CA 与 ServiceAccount 密钥不轮换（V1 不支持 CA 轮换，）：续期前后 CA 指纹必须一致",
		"kubelet 客户端证书（TLS bootstrap）与 kubelet serving 证书由 kubelet 自动轮换，不在此处理",
		"kubeconfig 引用证书文件而不是内嵌，因此续期不需要重写 kubeconfig",
	)

	// 证书选择：未点名时处理全部证书（各角色按存在性自然过滤），
	// 点名时只处理点名的证书。
	selected := map[pki.CertID]bool{}
	for _, name := range opts.CertIDs {
		leaf, ok := pki.DefaultCatalogue.FindLeaf(name)
		if !ok {
			return nil, fmt.Errorf("未知证书 %q；可选：%s", name, certNames())
		}
		selected[leaf.ID] = true
	}

	restart := map[string]bool{}
	// 上一次续期成功写证书、但重启失败时留下的组件：即使本次没有证书进入
	// 续期窗口，也必须再尝试重启一次。
	pending, err := readPending(opts.Paths)
	if err != nil {
		return nil, err
	}
	for _, component := range pending {
		restart[component] = true
	}
	if len(pending) > 0 {
		report.Notes = append(report.Notes, fmt.Sprintf("发现上次未完成的组件重启：%s（本次会重试）", strings.Join(pending, "、")))
	}
	for _, spec := range pki.DefaultCatalogue.AllLeaves() {
		if len(selected) > 0 && !selected[spec.ID] {
			continue
		}
		for _, component := range report.renewOne(opts, cas, spec, len(selected) > 0) {
			restart[component] = true
		}
	}

	// CA 指纹必须与续期前一致：续期绝不能悄悄换掉 CA。
	for _, ca := range cas {
		if after := fingerprintOfCertFile(ca.CertPath); after != before[ca.Name] {
			report.failures = append(report.failures,
				fmt.Errorf("CA %s 在续期过程中发生变化（%s → %s）：这不应该发生，请立即检查",
					ca.Name, before[ca.Name], after))
		}
	}

	if opts.DryRun {
		return report, joinErrors(report.failures)
	}
	if len(restart) > 0 {
		components := make([]string, 0, len(restart))
		for c := range restart {
			components = append(components, c)
		}
		sort.Strings(components)
		if opts.Restarter != nil {
			if err := restartComponents(ctx, opts, components); err != nil {
				report.failures = append(report.failures, err)
			} else {
				report.Restarted = components
				if err := clearPending(opts.Paths); err != nil {
					report.failures = append(report.failures, fmt.Errorf("清理证书待重启标记失败: %w", err))
				}
			}
		} else {
			message := fmt.Sprintf("未注入 Restarter：证书已重签，但组件未重启；请手动重启 %s", strings.Join(components, "、"))
			opts.Reporter().Warnf("%s", message)
			report.Notes = append(report.Notes, message)
		}
	}
	if report.Renewed == 0 && report.Failed == 0 {
		report.Notes = append(report.Notes, fmt.Sprintf("所有证书都在续期窗口（%s）之外，无需续期", opts.warnBefore()))
	}
	return report, joinErrors(report.failures)
}

// renewOne 处理一张叶子证书：存在性、到期窗口、签发者、dry-run、签发与写盘。
// 无论成败都把结果写进 report，返回需要重启的组件列表。
func (r *Report) renewOne(opts Options, cas []caRef, spec pki.Leaf, selected bool) []string {
	now, warnBefore, validity := opts.now(), opts.warnBefore(), opts.validity()
	certPath, keyPath := spec.Paths(opts.Paths)
	certExists := pki.FileExists(certPath)
	keyExists := pki.FileExists(keyPath)
	if !certExists || !keyExists {
		// 未显式选择时跳过：这些证书本来就不属于本机角色。但 `--cert`
		// 明确点名时必须失败——否则会退化成“成功且什么都没做”。
		if !selected {
			return nil
		}
		result := CertificateResult{Name: spec.CertFile, Path: certPath, KeyPath: keyPath}
		if !certExists && !keyExists {
			result.Error = "本机没有该证书文件"
		} else {
			result.Error = "证书与私钥必须成对存在（缺少其中一个）"
		}
		r.fail(result, fmt.Errorf("%s: %s", certPath, result.Error))
		return nil
	}

	result := CertificateResult{Name: spec.CertFile, Path: certPath, KeyPath: keyPath}
	leaf, err := pki.LoadCertificate(certPath)
	if err != nil {
		result.Error = err.Error()
		r.fail(result, fmt.Errorf("%s: %w", certPath, err))
		return nil
	}
	result.CommonName = leaf.Subject.CommonName
	result.Issuer = leaf.Issuer.CommonName
	result.OldNotAfter = leaf.NotAfter

	if !opts.Force && !expiresWithin(leaf, warnBefore, now) {
		result.Skipped = fmt.Sprintf("有效期至 %s，仍在续期窗口（%s）之外",
			leaf.NotAfter.UTC().Format("2006-01-02"), warnBefore)
		r.Skipped++
		r.Results = append(r.Results, result)
		return nil
	}
	issuer := findIssuer(cas, leaf)
	if issuer == nil {
		result.Error = fmt.Sprintf("找不到签发该证书的 CA（%s 未由本机 CA 签发，可能是企业 CA）", leaf.Issuer.CommonName)
		r.fail(result, fmt.Errorf("%s: %s", certPath, result.Error))
		return nil
	}
	if opts.DryRun {
		result.NewNotAfter = now.Add(validity)
		result.Renewed = true
		r.Renewed++
		r.Results = append(r.Results, result)
		opts.Reporter().Infof("将重签 %s（CA %s，dry-run）", certPath, issuer.Name)
		return nil
	}
	// 先落“待重启”标记，再写证书：写成功但随后重启失败/崩溃时，下一次
	// cert renew 仍知道这些组件需要重启。
	if err := addPending(opts.Paths, spec.Restart); err != nil {
		result.Error = fmt.Sprintf("记录待重启状态失败: %v", err)
		r.fail(result, fmt.Errorf("%s: %v", certPath, err))
		return nil
	}
	cert, key, err := issuer.CA.Sign(certConfigFromLeaf(leaf, validity))
	if err != nil {
		result.Error = fmt.Sprintf("重签失败: %v", err)
		r.fail(result, fmt.Errorf("%s: %v", certPath, err))
		return nil
	}
	if err := pki.WriteCertAndKey(certPath, keyPath, cert, key); err != nil {
		result.Error = err.Error()
		r.fail(result, err)
		return nil
	}
	result.Renewed = true
	result.NewNotAfter = cert.NotAfter
	result.Fingerprint = pki.Fingerprint(cert)
	r.Renewed++
	r.Results = append(r.Results, result)
	opts.Reporter().Infof("已重签 %s（%s → %s）", certPath,
		leaf.NotAfter.UTC().Format("2006-01-02"), cert.NotAfter.UTC().Format("2006-01-02"))
	return spec.Restart
}

// restartComponents 重启受影响的组件。
func restartComponents(ctx context.Context, opts Options, components []string) error {
	if opts.Restarter == nil {
		return nil
	}
	if err := opts.Restarter.Restart(ctx, components); err != nil {
		return fmt.Errorf("重启组件 %s 失败: %w", strings.Join(components, "、"), err)
	}
	opts.Reporter().Infof("已重启受影响的组件：%s", strings.Join(components, "、"))
	return nil
}

// certConfigFromLeaf 保留叶子证书的主题、SAN 与用途。
// 续期是"同一张证书、新的有效期"，不是重新设计 SAN；master 增减带来的 SAN
// 变化由 `certs` phase 处理。
func certConfigFromLeaf(leaf *x509.Certificate, validity time.Duration) pki.CertConfig {
	usages := leaf.ExtKeyUsage
	if len(usages) == 0 {
		usages = pki.UsageServerClient
	}
	return pki.CertConfig{
		CommonName:    leaf.Subject.CommonName,
		Organizations: append([]string(nil), leaf.Subject.Organization...),
		DNSNames:      append([]string(nil), leaf.DNSNames...),
		IPAddresses:   append([]net.IP(nil), leaf.IPAddresses...),
		Usages:        usages,
		Validity:      validity,
	}
}

// findIssuer 返回签发该叶子的 CA，找不到时返回 nil。
func findIssuer(cas []caRef, leaf *x509.Certificate) *caRef {
	for i := range cas {
		if err := leaf.CheckSignatureFrom(cas[i].CA.Cert); err == nil {
			return &cas[i]
		}
	}
	return nil
}

// expiresWithin 报告证书是否已进入续期窗口。
func expiresWithin(leaf *x509.Certificate, warnBefore time.Duration, now time.Time) bool {
	return !now.Add(warnBefore).Before(leaf.NotAfter)
}

// joinErrors 把多个失败合并成一个错误。
func joinErrors(errs []error) error {
	switch len(errs) {
	case 0:
		return nil
	case 1:
		return errs[0]
	default:
		parts := make([]string, 0, len(errs))
		for _, err := range errs {
			parts = append(parts, err.Error())
		}
		return fmt.Errorf("%s", strings.Join(parts, "；"))
	}
}
