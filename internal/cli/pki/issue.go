package pki

import (
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"kubemono/internal/cli/options"
	"kubemono/internal/cli/term"
	"kubemono/internal/cli/utils"
	pkipkg "kubemono/internal/pki"
)

// issueResult 是 `kubemono pki issue` 的输出。
type issueResult struct {
	CertPath    string    `json:"certPath"`
	KeyPath     string    `json:"keyPath"`
	Profile     string    `json:"profile"`
	CommonName  string    `json:"commonName"`
	DNSNames    []string  `json:"dnsNames,omitempty"`
	IPs         []string  `json:"ips,omitempty"`
	NotAfter    time.Time `json:"notAfter"`
	Fingerprint string    `json:"fingerprint"`
}

// issueLong 是 `kubemono pki issue` 的帮助说明。
const issueLong = `用 --ca/--ca-key 指定的 CA 签发一张叶子证书。

profile：server=ServerAuth、client=ClientAuth、peer=两者兼有；server/peer 必须
至少一个 --san。--cn 必填，同时作为输出文件名（<cn>.crt / <cn>.key）。
有效期自动钳制到 CA 到期时间，且拒绝覆盖已有证书/私钥。`

// issueOptions 是 `pki issue` 的全部输入。
type issueOptions struct {
	caPath    string
	caKeyPath string
	profile   string
	cn        string
	sans      []string
	orgs      []string
	days      int
	outDir    string
}

func newIssueCmd(opts *options.GlobalOptions) *cobra.Command {
	var o issueOptions
	cmd := &cobra.Command{
		Use:   "issue",
		Short: "用已有的 CA 签发一张叶子证书",
		Long:  issueLong,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runIssue(cmd, opts, &o)
		},
	}
	cmd.Flags().StringVar(&o.caPath, "ca", "", "CA 证书路径（必填）")
	cmd.Flags().StringVar(&o.caKeyPath, "ca-key", "", "CA 私钥路径（必填）")
	cmd.Flags().StringVar(&o.profile, "profile", "server", "用途：server|client|peer")
	cmd.Flags().StringVar(&o.cn, "cn", "", "证书 CN（必填），同时作为输出文件基名")
	cmd.Flags().StringSliceVar(&o.sans, "san", nil, "SAN，可重复/逗号分隔；支持 DNS 与 IP")
	cmd.Flags().StringSliceVar(&o.orgs, "organization", nil, "证书 O（可重复）")
	cmd.Flags().IntVar(&o.days, "days", 365, "有效期天数（自动钳制到 CA 到期时间）")
	cmd.Flags().StringVar(&o.outDir, "out-dir", ".", "输出目录")
	_ = cmd.MarkFlagRequired("ca")
	_ = cmd.MarkFlagRequired("ca-key")
	_ = cmd.MarkFlagRequired("cn")
	return options.Add(cmd, opts, options.OutputFlag)
}

// validate 归一化并校验 flag，顺便补齐 name/cn/outDir 的默认值。
func (o *issueOptions) validate() ([]string, []net.IP, []x509.ExtKeyUsage, error) {
	o.profile = strings.ToLower(strings.TrimSpace(o.profile))
	if strings.TrimSpace(o.caPath) == "" || strings.TrimSpace(o.caKeyPath) == "" {
		return nil, nil, nil, fmt.Errorf("--ca 与 --ca-key 必须同时提供")
	}
	usages, err := profileEKU(o.profile)
	if err != nil {
		return nil, nil, nil, err
	}
	dns, ips, err := parseSANs(o.sans)
	if err != nil {
		return nil, nil, nil, err
	}
	if (o.profile == "server" || o.profile == "peer") && len(dns)+len(ips) == 0 {
		return nil, nil, nil, fmt.Errorf("profile=%s 需要至少一个 --san（现代 TLS 只校验 SAN）", o.profile)
	}
	if strings.ContainsAny(o.cn, "/\\") || o.cn == "." || o.cn == ".." {
		return nil, nil, nil, fmt.Errorf("--cn 会作为输出文件名，不能包含路径分隔符：%q", o.cn)
	}
	if o.days <= 0 {
		return nil, nil, nil, fmt.Errorf("--days 必须是正整数")
	}
	if o.outDir == "" {
		o.outDir = "."
	}
	return dns, ips, usages, nil
}

// runIssue 是签发命令的编排：校验 → 加载 CA → 计算有效期 → 签发 → 落盘 → 输出。
func runIssue(cmd *cobra.Command, opts *options.GlobalOptions, o *issueOptions) error {
	dns, ips, usages, err := o.validate()
	if err != nil {
		return err
	}
	ca, err := pkipkg.LoadCA(o.caPath, o.caKeyPath)
	if err != nil {
		return fmt.Errorf("加载 CA 失败: %w", err)
	}
	validity, err := issueValidity(ca, o.days, o.caPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(o.outDir, 0o700); err != nil {
		return fmt.Errorf("创建输出目录 %s 失败: %w", o.outDir, err)
	}
	certPath := filepath.Join(o.outDir, o.cn+".crt")
	keyPath := filepath.Join(o.outDir, o.cn+".key")
	for _, path := range []string{certPath, keyPath} {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("%s 已存在；请先移走或换 --cn/--out-dir，本命令不覆盖已有文件", path)
		}
	}
	cert, key, err := ca.Sign(pkipkg.CertConfig{
		CommonName:    o.cn,
		Organizations: o.orgs,
		DNSNames:      dns,
		IPAddresses:   ips,
		Usages:        usages,
		Validity:      validity,
	})
	if err != nil {
		return fmt.Errorf("签发 %s 失败: %w", o.cn, err)
	}
	if err := pkipkg.WriteCertAndKey(certPath, keyPath, cert, key); err != nil {
		return err
	}
	info, err := pkipkg.Inspect(certPath)
	if err != nil {
		return err
	}
	return renderIssueResult(cmd, opts, issueResult{
		CertPath:    certPath,
		KeyPath:     keyPath,
		Profile:     o.profile,
		CommonName:  o.cn,
		DNSNames:    dns,
		IPs:         pkipkg.IPStrings(ips),
		NotAfter:    info[0].NotAfter,
		Fingerprint: info[0].Fingerprint,
	})
}

// issueValidity 把 --days 转换成有效期，并钳制到 CA 到期时间。
func issueValidity(ca *pkipkg.CA, days int, caPath string) (time.Duration, error) {
	remaining := time.Until(ca.Cert.NotAfter)
	if remaining <= 0 {
		return 0, fmt.Errorf("CA %s 已过期（%s）", caPath, ca.Cert.NotAfter.UTC().Format(time.RFC3339))
	}
	validity := time.Duration(days) * 24 * time.Hour
	if validity > remaining {
		validity = remaining - time.Minute
		if validity <= 0 {
			validity = remaining
		}
	}
	return validity, nil
}

// profileEKU 把 profile 转换为 ExtKeyUsage。
func profileEKU(profile string) ([]x509.ExtKeyUsage, error) {
	switch strings.ToLower(strings.TrimSpace(profile)) {
	case "", "server":
		return pkipkg.UsageServer, nil
	case "client":
		return pkipkg.UsageClient, nil
	case "peer":
		return pkipkg.UsageServerClient, nil
	default:
		return nil, fmt.Errorf("未知 profile %q（可选 server|client|peer）", profile)
	}
}

// parseSANs 把 --san 拆成 DNS 与 IP；拒绝当前不支持的类型。
func parseSANs(sans []string) ([]string, []net.IP, error) {
	var dns []string
	var ips []net.IP
	for _, raw := range sans {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		if strings.HasPrefix(s, "uri:") || strings.HasPrefix(s, "email:") {
			return nil, nil, fmt.Errorf("暂不支持 %s SAN（只支持 DNS/IP）：%q", strings.SplitN(s, ":", 2)[0], s)
		}
		if ip := net.ParseIP(s); ip != nil {
			ips = append(ips, ip)
			continue
		}
		if strings.ContainsAny(s, " \t") {
			return nil, nil, fmt.Errorf("非法 SAN %q：不能包含空白", s)
		}
		dns = append(dns, s)
	}
	return dns, ips, nil
}

// renderIssueResult 渲染签发结果。
func renderIssueResult(cmd *cobra.Command, opts *options.GlobalOptions, res issueResult) error {
	out := cmd.OutOrStdout()
	return utils.Print(out, opts.Output, res, func(w io.Writer) error {
		_, _ = fmt.Fprint(w, term.KeyValues(map[string]string{
			"cert":        res.CertPath,
			"key":         res.KeyPath,
			"profile":     res.Profile,
			"common name": res.CommonName,
			"dns":         strings.Join(res.DNSNames, ","),
			"ips":         strings.Join(res.IPs, ","),
			"not after":   res.NotAfter.UTC().Format("2006-01-02T15:04:05Z"),
			"fingerprint": res.Fingerprint,
		}))
		return nil
	})
}
