package certrenew

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"k8s.io/client-go/tools/clientcmd"

	"kubemono/internal/constants"
	"kubemono/internal/pki"
)

// sourcePriority 决定聚合后“主要来源”的取值：PKI 扫描是权威来源。
var sourcePriority = map[string]int{"pki": 0, "etcd-pki": 1, "kubeconfig": 2}

// CheckExpiration 扫描 PKI 目录和 kubeconfig 引用的证书，报告剩余有效期与告警窗口。
// 只读：不写任何文件。warn 表示证书进入续期窗口（默认 30 天），
// expired 表示已经过期。
//
// 结果按“证书身份”（文件路径 + 证书指纹）聚合：同一张证书被 pki 与多个
// kubeconfig 引用时只出一行，引用它的 kubeconfig 收进 Kubeconfigs。
// kubeconfig 引用的证书与 kubemono 期望不一致时（路径不对、内嵌内容不同、集群 CA
// 不同），该引用作为独立条目出现并带 Mismatch 说明，不会被聚合成同一条。
func CheckExpiration(_ context.Context, opts Options) ([]CertStatus, error) {
	now := opts.now()
	warnBefore := opts.warnBefore()
	exp := newExpectations(opts.Paths)
	// 非 nil 空切片：JSON 输出必须是 []，而不是 null。
	scanned := []CertStatus{}

	dirs := []struct {
		path   string
		source string
	}{
		{opts.Paths.PKI(), "pki"},
		{opts.Paths.EtcdPKI(), "etcd-pki"},
	}
	for _, dir := range dirs {
		matches, err := filepath.Glob(filepath.Join(dir.path, "*.crt"))
		if err != nil {
			return nil, fmt.Errorf("扫描 %s 失败: %w", dir.path, err)
		}
		sort.Strings(matches)
		for _, p := range matches {
			scanned = append(scanned, statusOfFile(p, filepath.Base(p), dir.source, "", now, warnBefore)...)
		}
	}

	kubeconfigs, err := filepath.Glob(filepath.Join(opts.Paths.Kubernetes(), "*.conf"))
	if err != nil {
		return nil, fmt.Errorf("扫描 kubeconfig 失败: %w", err)
	}
	sort.Strings(kubeconfigs)
	for _, kc := range kubeconfigs {
		scanned = append(scanned, exp.kubeconfigStatuses(kc, now, warnBefore)...)
	}

	out := aggregate(scanned)
	sortStatuses(out)
	return out, nil
}

// statusOfFile 读取证书文件，为每张证书生成一条状态。
func statusOfFile(path, name, source, kubeconfig string, now time.Time, warnBefore time.Duration) []CertStatus {
	data, err := os.ReadFile(path)
	if err != nil {
		return []CertStatus{{Path: path, Name: name, Source: source, kubeconfig: kubeconfig, Error: fmt.Sprintf("读取失败: %v", err)}}
	}
	return statusOfBytes(path, name, source, kubeconfig, data, now, warnBefore)
}

// statusOfBytes 解析 PEM 字节，为每张证书生成一条状态。
func statusOfBytes(path, name, source, kubeconfig string, data []byte, now time.Time, warnBefore time.Duration) []CertStatus {
	certs, err := pki.ParseCertsPEM(data)
	if err != nil {
		return []CertStatus{{Path: path, Name: name, Source: source, kubeconfig: kubeconfig, Error: err.Error()}}
	}
	if len(certs) == 0 {
		return []CertStatus{{Path: path, Name: name, Source: source, kubeconfig: kubeconfig, Error: "文件里没有证书"}}
	}
	out := make([]CertStatus, 0, len(certs))
	for _, cert := range certs {
		remaining := cert.NotAfter.Sub(now)
		status := CertStatus{
			Path:        path,
			Name:        name,
			Source:      source,
			kubeconfig:  kubeconfig,
			fingerprint: pki.Fingerprint(cert),
			CommonName:  cert.Subject.CommonName,
			Issuer:      cert.Issuer.CommonName,
			IsCA:        cert.IsCA,
			NotAfter:    cert.NotAfter,
			Remaining:   humanDuration(remaining),
		}
		switch {
		case remaining <= 0:
			status.Expired = true
		case remaining <= warnBefore:
			status.Warn = true
		}
		out = append(out, status)
	}
	return out
}

// expectations 描述“受管 kubeconfig 应该引用哪张证书”，用于发现不一致。
type expectations struct {
	paths constants.Paths
	// byConf 是 kubeconfig 文件名到期望的客户端证书路径的映射（来自 PKI catalogue）。
	byConf map[string]string
	// caPath 是本机集群 CA（kubeconfig 的 cluster CA 应该指向它）。
	caPath string
	// fingerprints 缓存“路径 → 第一张证书指纹”，避免重复读盘。
	fingerprints map[string]string
}

// newExpectations 从 PKI catalogue 推导每份 kubeconfig 期望引用的证书路径。
// worker 的 kubelet.conf 例外，见 kubeletManaged。
func newExpectations(paths constants.Paths) *expectations {
	e := &expectations{
		paths:        paths,
		byConf:       map[string]string{},
		caPath:       paths.CertPath(constants.CACertName),
		fingerprints: map[string]string{},
	}
	for _, leaf := range pki.DefaultCatalogue.AllLeaves() {
		if leaf.KubeconfigFile == "" {
			continue
		}
		certPath, _ := leaf.Paths(paths)
		e.byConf[leaf.KubeconfigFile] = certPath
	}
	return e
}

// fingerprintOfFile 返回文件里第一张证书的指纹；读不到或解析失败时返回空串。
func (e *expectations) fingerprintOfFile(path string) string {
	if v, ok := e.fingerprints[path]; ok {
		return v
	}
	sum := fingerprintOfCertFile(path)
	e.fingerprints[path] = sum
	return sum
}

// caFingerprint 返回本机集群 CA 的指纹。
func (e *expectations) caFingerprint() string { return e.fingerprintOfFile(e.caPath) }

// kubeletManaged 报告路径是否在 kubelet 自己维护的 PKI 目录里。
// worker 的 kubelet.conf 由 kubelet 在 TLS Bootstrap 后自己写入并轮换，引用
// <kubelet>/pki/kubelet-client-current.pem；这与 kubemono 管理的 kubelet-client.crt
// 不同，但完全合法，不能算不一致。
func (e *expectations) kubeletManaged(path string) bool {
	dir := filepath.Join(e.paths.Kubelet(), "pki")
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// kubeconfigStatuses 报告单个 kubeconfig 引用的证书，并标出与期望不一致的引用。
func (e *expectations) kubeconfigStatuses(path string, now time.Time, warnBefore time.Duration) []CertStatus {
	cfg, err := clientcmd.LoadFromFile(path)
	if err != nil {
		return []CertStatus{{Path: path, Name: filepath.Base(path), Source: "kubeconfig", Error: err.Error()}}
	}
	base := filepath.Dir(path)
	confBase := filepath.Base(path)
	resolve := func(p string) string {
		if p == "" || filepath.IsAbs(p) {
			return p
		}
		return filepath.Join(base, p)
	}
	expectCert, managed := e.byConf[confBase]

	var out []CertStatus
	authNames := make([]string, 0, len(cfg.AuthInfos))
	for name := range cfg.AuthInfos {
		authNames = append(authNames, name)
	}
	sort.Strings(authNames)
	for _, name := range authNames {
		auth := cfg.AuthInfos[name]
		if auth == nil {
			continue
		}
		switch {
		case auth.ClientCertificate != "":
			p := resolve(auth.ClientCertificate)
			got := statusOfFile(p, name, "kubeconfig", path, now, warnBefore)
			out = append(out, e.checkClientRef(confBase, managed, expectCert, p, false, got)...)
		case len(auth.ClientCertificateData) > 0:
			p := path + "#" + name
			got := statusOfBytes(p, name, "kubeconfig", path, auth.ClientCertificateData, now, warnBefore)
			out = append(out, e.checkClientRef(confBase, managed, expectCert, p, true, got)...)
		}
	}

	clusterNames := make([]string, 0, len(cfg.Clusters))
	for name := range cfg.Clusters {
		clusterNames = append(clusterNames, name)
	}
	sort.Strings(clusterNames)
	for _, name := range clusterNames {
		cluster := cfg.Clusters[name]
		if cluster == nil {
			continue
		}
		switch {
		case cluster.CertificateAuthority != "":
			p := resolve(cluster.CertificateAuthority)
			out = append(out, e.checkCARef(confBase, statusOfFile(p, name+"-ca", "kubeconfig", path, now, warnBefore))...)
		case len(cluster.CertificateAuthorityData) > 0:
			p := path + "#" + name + "-ca"
			out = append(out, e.checkCARef(confBase, statusOfBytes(p, name+"-ca", "kubeconfig", path, cluster.CertificateAuthorityData, now, warnBefore))...)
		}
	}
	return out
}

// checkClientRef 比较 kubeconfig 里的客户端证书与 kubemono 期望的那张。
func (e *expectations) checkClientRef(confBase string, managed bool, expectPath, actual string, embedded bool, statuses []CertStatus) []CertStatus {
	if !managed || expectPath == "" || len(statuses) == 0 {
		return statuses
	}
	switch {
	case actual == expectPath:
		return statuses
	case !embedded && confBase == constants.KubeletKubeconfigName && e.kubeletManaged(actual):
		return statuses
	case embedded:
		want := e.fingerprintOfFile(expectPath)
		got := statuses[0].fingerprint
		switch {
		case want == "":
			return markMismatch(statuses, fmt.Sprintf("%s 内嵌客户端证书，但 %s 不存在或无法解析", confBase, expectPath))
		case got != "" && got != want:
			return markMismatch(statuses, fmt.Sprintf("%s 内嵌的客户端证书与 %s 不一致", confBase, expectPath))
		}
		return statuses
	default:
		return markMismatch(statuses, fmt.Sprintf("%s 引用 %s，kubemono 期望 %s", confBase, actual, expectPath))
	}
}

// checkCARef 比较 kubeconfig 的集群 CA 与本机 ca.crt。
func (e *expectations) checkCARef(confBase string, statuses []CertStatus) []CertStatus {
	want := e.caFingerprint()
	if want == "" || len(statuses) == 0 || statuses[0].Error != "" {
		return statuses
	}
	if got := statuses[0].fingerprint; got != "" && got != want {
		return markMismatch(statuses, fmt.Sprintf("%s 的集群 CA 与 %s 不一致", confBase, e.caPath))
	}
	return statuses
}

// markMismatch 给可解析的条目加上不一致说明。
func markMismatch(statuses []CertStatus, reason string) []CertStatus {
	for i := range statuses {
		if statuses[i].Error == "" && statuses[i].Mismatch == "" {
			statuses[i].Mismatch = reason
		}
	}
	return statuses
}

// aggregate 按“路径 + 证书指纹”合并同一张证书的多次引用。
// 指纹为空的错误条目按“路径 + 错误文本”合并。
func aggregate(scanned []CertStatus) []CertStatus {
	type key struct {
		path        string
		fingerprint string
		errText     string
	}
	out := make([]CertStatus, 0, len(scanned))
	index := map[key]int{}
	for _, st := range scanned {
		k := key{path: st.Path, fingerprint: st.fingerprint}
		if st.fingerprint == "" {
			k.errText = st.Error
		}
		if i, ok := index[k]; ok {
			mergeStatus(&out[i], st)
			continue
		}
		st.Sources = []string{st.Source}
		if st.kubeconfig != "" {
			st.Kubeconfigs = []string{st.kubeconfig}
		}
		out = append(out, st)
		index[k] = len(out) - 1
	}
	for i := range out {
		slices.SortFunc(out[i].Sources, func(a, b string) int {
			return sourcePriority[a] - sourcePriority[b]
		})
		out[i].Source = out[i].Sources[0]
		sort.Strings(out[i].Kubeconfigs)
	}
	return out
}

// mergeStatus 把同一张证书的另一条引用合并进 target。
func mergeStatus(target *CertStatus, src CertStatus) {
	if !slices.Contains(target.Sources, src.Source) {
		target.Sources = append(target.Sources, src.Source)
	}
	if src.kubeconfig != "" && !slices.Contains(target.Kubeconfigs, src.kubeconfig) {
		target.Kubeconfigs = append(target.Kubeconfigs, src.kubeconfig)
	}
	switch {
	case src.Mismatch == "":
	case target.Mismatch == "":
		target.Mismatch = src.Mismatch
	case !strings.Contains(target.Mismatch, src.Mismatch):
		target.Mismatch += "；" + src.Mismatch
	}
}

// sortStatuses 按紧急程度排序报告：已过期、不一致、告警，然后按到期时间。
func sortStatuses(items []CertStatus) {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.Expired != b.Expired {
			return a.Expired
		}
		if (a.Mismatch != "") != (b.Mismatch != "") {
			return a.Mismatch != ""
		}
		if a.Warn != b.Warn {
			return a.Warn
		}
		if !a.NotAfter.Equal(b.NotAfter) {
			return a.NotAfter.Before(b.NotAfter)
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Name < b.Name
	})
}

// humanDuration 紧凑地渲染剩余有效期。
func humanDuration(d time.Duration) string {
	negative := d < 0
	if negative {
		d = -d
	}
	var text string
	switch {
	case d >= 24*time.Hour:
		text = fmt.Sprintf("%dd", int(d.Hours()/24))
	case d >= time.Hour:
		text = fmt.Sprintf("%dh", int(d.Hours()))
	case d >= time.Minute:
		text = fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		text = fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if negative {
		return text + "（已过期）"
	}
	return text
}

// Expiring 返回处于告警窗口内（或已过期、读取失败）的证书。
// 计数按聚合后的证书身份，不会因为同一张 CA 被多个 kubeconfig 引用而虚高。
func Expiring(items []CertStatus) []CertStatus {
	var out []CertStatus
	for _, it := range items {
		if it.Warn || it.Expired || it.Error != "" {
			out = append(out, it)
		}
	}
	return out
}

// FormatRemaining 是 CLI 表格用的小工具。
func FormatRemaining(status CertStatus) string {
	if status.Error != "" {
		return "读取失败"
	}
	if status.Expired {
		return strings.TrimSpace(status.Remaining)
	}
	if status.Warn {
		return status.Remaining + "（告警）"
	}
	return status.Remaining
}
