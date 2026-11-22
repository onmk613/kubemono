package etcd

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
)

// TLSFiles 是访问 etcd 的 TLS 三件套。
// stacked 用本机的 pki/etcd/kubemono-client.*（独立客户端证书，
// 见 与 internal/join/seal 的密钥包）；external+unmanaged 用用户提供的文件。
type TLSFiles struct {
	// CAFile 是签发 etcd 服务端证书的 CA。
	CAFile string
	// CertFile / KeyFile 是客户端证书与私钥。
	CertFile string
	KeyFile  string
}

// Empty 判断是否没有配置任何 TLS 文件（即明文 etcd）。
func (t TLSFiles) Empty() bool {
	return t.CAFile == "" && t.CertFile == "" && t.KeyFile == ""
}

// Validate 拒绝只配置一半的 TLS：缺一个文件就会退化成“静默明文”，
// 因此三件套必须同时给全。
func (t TLSFiles) Validate() error {
	if t.Empty() {
		return nil
	}
	missing := make([]string, 0, 3)
	if t.CAFile == "" {
		missing = append(missing, "caFile")
	}
	if t.CertFile == "" {
		missing = append(missing, "certFile")
	}
	if t.KeyFile == "" {
		missing = append(missing, "keyFile")
	}
	if len(missing) > 0 {
		return fmt.Errorf("etcd TLS 配置不完整，缺少 %v：三件套（CA、客户端证书、私钥）必须同时提供", missing)
	}
	return nil
}

// tlsConfig 用三件套文件构造客户端 tls.Config。
// 不用 go.etcd.io/etcd/client/pkg/v3/transport：它只是为了拼同样的
// *tls.Config，而标准库几行就能做到，可以让 internal/etcd 的依赖面保持最小。
func tlsConfig(files TLSFiles) (*tls.Config, error) {
	if err := files.Validate(); err != nil {
		return nil, err
	}
	if files.Empty() {
		return nil, nil
	}
	cert, err := tls.LoadX509KeyPair(files.CertFile, files.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("读取 etcd 客户端证书（%s / %s）失败：%w；确认 certs phase 已在本机完成",
			files.CertFile, files.KeyFile, err)
	}
	caData, err := os.ReadFile(files.CAFile)
	if err != nil {
		return nil, fmt.Errorf("读取 etcd CA（%s）失败：%w；确认 certs phase 已在本机完成", files.CAFile, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caData) {
		return nil, fmt.Errorf("解析 etcd CA %s 失败：文件里没有可用的 PEM 证书", files.CAFile)
	}
	cfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS12,
	}
	return cfg, nil
}
