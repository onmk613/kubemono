package cert

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"kubemono/internal/cli/options"
	"kubemono/internal/cli/term"
	"kubemono/internal/cli/utils"
	"kubemono/internal/pki"
)

// showLong 是 `kubemono cert show` 的帮助说明。
const showLong = `只读解析 PEM 证书文件，不读取私钥、不修改任何文件。

支持一个文件内包含多张证书的链；输出 Subject、Issuer、SAN、KeyUsage/EKU、
有效期、签名算法与 SHA256 指纹。`

// newCertShowCmd 实现 `kubemono cert show`：只读解析 PEM 证书（可含链）。
func newCertShowCmd(opts *options.GlobalOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <cert.pem> [cert.pem ...]",
		Short: "查看证书内容（Subject/SAN/EKU/有效期/指纹/链）",
		Long:  showLong,
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			all := make([]pki.CertInfo, 0, len(args))
			for _, path := range args {
				infos, err := pki.Inspect(path)
				if err != nil {
					return err
				}
				all = append(all, infos...)
			}
			out := cmd.OutOrStdout()
			return utils.Print(out, opts.Output, all, func(w io.Writer) error {
				for _, info := range all {
					_, _ = fmt.Fprintf(w, "== %s [#%d] ==\n", info.Path, info.Index)
					_, _ = fmt.Fprint(w, term.KeyValues(certInfoPairs(info)))
					_, _ = fmt.Fprintln(w)
				}
				return nil
			})
		},
	}
	return options.Add(cmd, opts, options.OutputFlag)
}

// certInfoPairs 把证书信息转换为稳定的键值对。
func certInfoPairs(info pki.CertInfo) map[string]string {
	pairs := map[string]string{
		"subject":       info.Subject,
		"issuer":        info.Issuer,
		"self-signed":   fmt.Sprintf("%t", info.SelfSigned),
		"serial":        info.Serial,
		"not before":    info.NotBefore.UTC().Format("2006-01-02T15:04:05Z"),
		"not after":     info.NotAfter.UTC().Format("2006-01-02T15:04:05Z"),
		"is CA":         fmt.Sprintf("%t", info.IsCA),
		"path len":      fmt.Sprintf("%d", info.PathLen),
		"path len zero": fmt.Sprintf("%t", info.PathLenZero),
		"key usage":     strings.Join(info.KeyUsage, ","),
		"ext key usage": strings.Join(info.ExtKeyUsage, ","),
		"dns names":     strings.Join(info.DNSNames, ","),
		"ip addresses":  strings.Join(info.IPs, ","),
		"uris":          strings.Join(info.URIs, ","),
		"emails":        strings.Join(info.Emails, ","),
		"signature":     info.Signature,
		"public key":    info.PublicKey,
		"fingerprint":   info.Fingerprint,
	}
	for k, v := range pairs {
		if v == "" {
			delete(pairs, k)
		}
	}
	return pairs
}
