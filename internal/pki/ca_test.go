package pki

import (
	"crypto"
	"crypto/x509"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kubemono/internal/constants"
)

func TestNewCA(t *testing.T) {
	tests := []struct {
		name        string
		cn          string
		alg         KeyAlgorithm
		validity    time.Duration
		wantAlg     KeyAlgorithm
		wantValid   time.Duration
		wantErr     error
		wantErrText string
	}{
		{name: "默认算法与默认 10 年", cn: "kubernetes-ca", alg: "", validity: 0, wantAlg: RSA2048, wantValid: constants.DefaultCATTL},
		{name: "ecdsa 显式有效期", cn: "front-proxy-ca", alg: ECDSAP256, validity: 48 * time.Hour, wantAlg: ECDSAP256, wantValid: 48 * time.Hour},
		{name: "负有效期回落到默认", cn: "etcd-ca", alg: ECDSAP256, validity: -time.Hour, wantAlg: ECDSAP256, wantValid: constants.DefaultCATTL},
		{name: "空 CN", cn: "  ", alg: ECDSAP256, wantErrText: "CN"},
		{name: "不支持的算法", cn: "kubernetes-ca", alg: KeyAlgorithm("ed25519"), wantErr: ErrUnknownAlgorithm},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ca, err := NewCA(tt.cn, tt.alg, tt.validity)
			if tt.wantErrText != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrText) {
					t.Fatalf("期望包含 %q 的错误，得到 %v", tt.wantErrText, err)
				}
				return
			}
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("期望 %v，得到 %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewCA() 失败: %v", err)
			}
			if ca.Cert.Subject.CommonName != tt.cn {
				t.Fatalf("CN = %q, 期望 %q", ca.Cert.Subject.CommonName, tt.cn)
			}
			if !ca.Cert.IsCA || !ca.Cert.BasicConstraintsValid {
				t.Fatal("新 CA 必须具备 IsCA 与 BasicConstraints")
			}
			if ca.Cert.KeyUsage&x509.KeyUsageCertSign == 0 {
				t.Fatal("新 CA 必须允许签发证书")
			}
			if err := ca.Cert.CheckSignatureFrom(ca.Cert); err != nil {
				t.Fatalf("CA 不是自签: %v", err)
			}
			alg, err := KeyAlgorithmOf(ca.Key)
			if err != nil {
				t.Fatal(err)
			}
			if alg != tt.wantAlg {
				t.Fatalf("算法 = %q, 期望 %q", alg, tt.wantAlg)
			}
			gotValid := ca.Cert.NotAfter.Sub(ca.Cert.NotBefore)
			if diff := gotValid - (tt.wantValid + clockSkew); diff > time.Second || diff < -time.Second {
				t.Fatalf("有效期 = %s, 期望约 %s", gotValid, tt.wantValid+clockSkew)
			}
			if !ca.Cert.NotBefore.Before(time.Now()) {
				t.Fatal("NotBefore 应该回拨时钟偏移")
			}
		})
	}
}

func TestCAWriteToDiskAndLoadCA(t *testing.T) {
	for _, alg := range []KeyAlgorithm{ECDSAP256, RSA2048} {
		t.Run(string(alg), func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "nested", "pki")
			certPath := filepath.Join(dir, constants.CACertName)
			keyPath := filepath.Join(dir, constants.CAKeyName)

			ca, err := NewCA("kubernetes-ca", alg, time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			if err := ca.WriteToDisk(certPath, keyPath); err != nil {
				t.Fatalf("WriteToDisk() 失败: %v", err)
			}
			assertMode(t, certPath, 0o644)
			assertMode(t, keyPath, 0o600)
			assertMode(t, dir, 0o700)
			assertNoTempFiles(t, dir)

			loaded, err := LoadCA(certPath, keyPath)
			if err != nil {
				t.Fatalf("LoadCA() 失败: %v", err)
			}
			if loaded.Cert.Subject.CommonName != "kubernetes-ca" {
				t.Fatalf("CN = %q", loaded.Cert.Subject.CommonName)
			}
			if !publicKeysEqual(loaded.Key.Public(), ca.Key.Public()) {
				t.Fatal("LoadCA 读回的私钥与写入的不一致")
			}

			// 覆盖写：结果仍然完整且没有残留临时文件。
			if err := ca.WriteToDisk(certPath, keyPath); err != nil {
				t.Fatalf("覆盖 WriteToDisk() 失败: %v", err)
			}
			if _, err := LoadCA(certPath, keyPath); err != nil {
				t.Fatalf("覆盖后 LoadCA() 失败: %v", err)
			}
			assertNoTempFiles(t, dir)
		})
	}
}

func TestWriteToDiskErrors(t *testing.T) {
	tests := []struct {
		name    string
		ca      *CA
		wantErr error
	}{
		{name: "nil CA", ca: nil, wantErr: ErrNoCA},
		{name: "缺少证书", ca: &CA{Key: mustSigner(t)}, wantErr: ErrNoCertificate},
		{name: "缺少私钥", ca: &CA{Cert: cloneCA(t, ECDSAP256).Cert}, wantErr: ErrNoPrivateKey},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			err := tt.ca.WriteToDisk(filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key"))
			if err == nil {
				t.Fatal("期望错误")
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("期望 %v，得到 %v", tt.wantErr, err)
			}
			entries, readErr := os.ReadDir(dir)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(entries) != 0 {
				t.Fatalf("失败时不应留下文件: %v", entries)
			}
		})
	}
}

func TestLoadCAErrors(t *testing.T) {
	ca := cloneCA(t, ECDSAP256)
	now := time.Now()
	dir := t.TempDir()
	goodCert := filepath.Join(dir, "ca.crt")
	goodKey := filepath.Join(dir, "ca.key")
	if err := ca.WriteToDisk(goodCert, goodKey); err != nil {
		t.Fatal(err)
	}
	otherKeyDir := t.TempDir()
	otherCert, _, _ := generatedKeyPair(t, otherKeyDir, "other-ca", now.Add(-time.Hour), now.Add(time.Hour), true)
	leafCertPath, leafKeyPath, _ := generatedKeyPair(t, t.TempDir(), "leaf", now.Add(-time.Hour), now.Add(time.Hour), false)
	expiredCert, expiredKey, _ := generatedKeyPair(t, t.TempDir(), "expired-ca", now.Add(-48*time.Hour), now.Add(-time.Hour), true)
	brokenCert := filepath.Join(t.TempDir(), "broken.crt")
	mustWriteFile(t, brokenCert, tamper(EncodeCertPEM(ca.Cert)), 0o644)

	tests := []struct {
		name     string
		certPath string
		keyPath  string
		wantErr  error
	}{
		{name: "证书不存在", certPath: filepath.Join(dir, "missing.crt"), keyPath: goodKey},
		{name: "私钥不存在", certPath: goodCert, keyPath: filepath.Join(dir, "missing.key")},
		{name: "私钥与证书不匹配", certPath: goodCert, keyPath: filepath.Join(otherKeyDir, "test.key")},
		{name: "证书被篡改", certPath: brokenCert, keyPath: goodKey},
		{name: "不是 CA", certPath: leafCertPath, keyPath: leafKeyPath, wantErr: ErrNotCA},
		{name: "CA 已过期", certPath: expiredCert, keyPath: expiredKey, wantErr: ErrCertExpired},
		{name: "证书与私钥来自不同 CA", certPath: otherCert, keyPath: goodKey, wantErr: ErrKeyMismatch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadCA(tt.certPath, tt.keyPath)
			if err == nil {
				t.Fatal("期望错误")
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("期望 %v，得到 %v", tt.wantErr, err)
			}
		})
	}
}

func TestCASign(t *testing.T) {
	for _, alg := range []KeyAlgorithm{ECDSAP256, RSA2048} {
		t.Run(string(alg), func(t *testing.T) {
			ca := cloneCA(t, alg)
			validity := 90 * 24 * time.Hour
			cert, key, err := ca.Sign(CertConfig{
				CommonName:    "kube-apiserver",
				Organizations: []string{"system:masters", "system:masters", " group-b "},
				DNSNames:      []string{"kubernetes", "Kubernetes", "kubernetes.default", " "},
				IPAddresses:   []net.IP{net.ParseIP("10.0.0.1"), net.ParseIP("10.0.0.1"), net.ParseIP("::1")},
				Validity:      validity,
			})
			if err != nil {
				t.Fatalf("Sign() 失败: %v", err)
			}
			if key == nil {
				t.Fatal("PublicKey 为空时应返回新生成的私钥")
			}
			if !publicKeysEqual(key.Public(), cert.PublicKey) {
				t.Fatal("返回的私钥与证书公钥不匹配")
			}
			if got := cert.Subject.CommonName; got != "kube-apiserver" {
				t.Fatalf("CN = %q", got)
			}
			wantOrgs := []string{"group-b", "system:masters"}
			if len(cert.Subject.Organization) != len(wantOrgs) {
				t.Fatalf("O = %v, 期望 %v（去重排序）", cert.Subject.Organization, wantOrgs)
			}
			for i, o := range wantOrgs {
				if cert.Subject.Organization[i] != o {
					t.Fatalf("O = %v, 期望 %v", cert.Subject.Organization, wantOrgs)
				}
			}
			wantDNS := []string{"kubernetes", "kubernetes.default"}
			if len(cert.DNSNames) != len(wantDNS) {
				t.Fatalf("DNS SAN = %v, 期望 %v（去重）", cert.DNSNames, wantDNS)
			}
			for i, d := range wantDNS {
				if cert.DNSNames[i] != d {
					t.Fatalf("DNS SAN = %v, 期望 %v", cert.DNSNames, wantDNS)
				}
			}
			if len(cert.IPAddresses) != 2 {
				t.Fatalf("IP SAN = %v, 期望 2 个（去重）", cert.IPAddresses)
			}
			if !cert.IPAddresses[0].Equal(net.ParseIP("10.0.0.1")) {
				t.Fatalf("IP SAN[0] = %v", cert.IPAddresses[0])
			}
			if len(cert.ExtKeyUsage) != 2 {
				t.Fatalf("默认用途 = %v, 期望服务端+客户端", cert.ExtKeyUsage)
			}
			if cert.IsCA {
				t.Fatal("叶子证书不应是 CA")
			}
			if err := cert.CheckSignatureFrom(ca.Cert); err != nil {
				t.Fatalf("叶子证书不是由该 CA 签发: %v", err)
			}
			gotValidity := cert.NotAfter.Sub(cert.NotBefore)
			if diff := gotValidity - (validity + clockSkew); diff > time.Second || diff < -time.Second {
				t.Fatalf("有效期 = %s, 期望约 %s", gotValidity, validity+clockSkew)
			}
		})
	}
}

func TestCASignConfigVariants(t *testing.T) {
	ca := cloneCA(t, ECDSAP256)
	providedKey, err := RSA2048.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		cfg        CertConfig
		wantNilKey bool
		wantErrMsg string
	}{
		{name: "空 CN", cfg: CertConfig{}, wantErrMsg: "CN"},
		{name: "提供公钥", cfg: CertConfig{CommonName: "provided", PublicKey: providedKey.Public()}, wantNilKey: true},
		{name: "客户端用途", cfg: CertConfig{CommonName: "client", Usages: UsageClient}},
		{name: "服务端用途", cfg: CertConfig{CommonName: "server", Usages: UsageServer}},
		{name: "负有效期取默认", cfg: CertConfig{CommonName: "default-ttl", Validity: -time.Minute}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cert, key, err := ca.Sign(tt.cfg)
			if tt.wantErrMsg != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrMsg) {
					t.Fatalf("期望包含 %q 的错误，得到 %v", tt.wantErrMsg, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Sign() 失败: %v", err)
			}
			if tt.wantNilKey {
				if key != nil {
					t.Fatal("提供 PublicKey 时不应返回私钥")
				}
				if !publicKeysEqual(cert.PublicKey, providedKey.Public()) {
					t.Fatal("证书公钥应等于调用方提供的公钥")
				}
				return
			}
			if key == nil {
				t.Fatal("未提供 PublicKey 时应返回新私钥")
			}
			if len(tt.cfg.Usages) > 0 && len(cert.ExtKeyUsage) != len(tt.cfg.Usages) {
				t.Fatalf("ExtKeyUsage = %v, 期望 %v", cert.ExtKeyUsage, tt.cfg.Usages)
			}
			if tt.cfg.Validity < 0 {
				if got := cert.NotAfter.Sub(cert.NotBefore); got < constants.DefaultCertTTL {
					t.Fatalf("有效期 = %s, 期望回落到 %s", got, constants.DefaultCertTTL)
				}
			}
		})
	}

	var nilCA *CA
	if _, _, err := nilCA.Sign(CertConfig{CommonName: "x"}); !errors.Is(err, ErrNoCA) {
		t.Fatalf("nil CA 应返回 ErrNoCA，得到 %v", err)
	}
}

func TestWrongCARejectsLeaf(t *testing.T) {
	ca := cloneCA(t, ECDSAP256)
	other, err := NewCA("other-ca", ECDSAP256, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if publicKeysEqual(ca.Cert.PublicKey, other.Cert.PublicKey) {
		t.Fatal("测试前提错误：两个 CA 不应相同")
	}
	leaf, _, err := ca.Sign(CertConfig{CommonName: "leaf", Validity: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err := leaf.CheckSignatureFrom(other.Cert); err == nil {
		t.Fatal("叶子证书不应通过另一个 CA 的签名校验")
	}
	if err := leaf.CheckSignatureFrom(ca.Cert); err != nil {
		t.Fatalf("用正确 CA 校验应成功: %v", err)
	}
}

func TestUnsupportedAlgorithmError(t *testing.T) {
	if _, err := (KeyAlgorithm("ed25519")).NewPrivateKey(); err == nil {
		t.Fatal("未知算法生成私钥应失败")
	}
}

func mustSigner(t *testing.T) crypto.Signer {
	t.Helper()
	key, err := ECDSAP256.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s 失败: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s 权限 = %o, 期望 %o", path, got, want)
	}
}

func assertNoTempFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") && strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("目录 %s 残留临时文件 %s", dir, e.Name())
		}
	}
}
