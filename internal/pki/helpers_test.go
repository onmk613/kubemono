package pki

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// 测试用的共享 CA。生成密钥有成本，同一算法的 CA 在所有用例间复用。
var (
	testCAMu    sync.Mutex
	testCACache = map[KeyAlgorithm]*CA{}
)

// sharedCA 返回（必要时创建）指定算法的测试 CA。
func sharedCA(t *testing.T, alg KeyAlgorithm) *CA {
	t.Helper()
	if alg == "" {
		alg = DefaultKeyAlgorithm
	}
	testCAMu.Lock()
	defer testCAMu.Unlock()
	if ca, ok := testCACache[alg]; ok {
		return ca
	}
	ca, err := NewCA("kubernetes-ca", alg, 10*365*24*time.Hour)
	if err != nil {
		t.Fatalf("创建测试 CA 失败: %v", err)
	}
	testCACache[alg] = ca
	return ca
}

// cloneCA 返回共享 CA 的浅拷贝，避免用例互相污染。
func cloneCA(t *testing.T, alg KeyAlgorithm) *CA {
	t.Helper()
	base := sharedCA(t, alg)
	return &CA{Cert: base.Cert, Key: base.Key}
}

// mustWriteFile 写文件并创建父目录。
func mustWriteFile(t *testing.T, path string, data []byte, perm os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	if err := os.WriteFile(path, data, perm); err != nil {
		t.Fatalf("写文件 %s 失败: %v", path, err)
	}
}

// selfSigned 生成一张自签证书（测试专用，不受被测代码的默认值约束，
// 因此可以构造过期、非 CA、不匹配等对抗性输入）。
func selfSigned(t *testing.T, key crypto.Signer, tmpl *x509.Certificate, parent *x509.Certificate, parentKey crypto.Signer) []byte {
	t.Helper()
	if key == nil {
		var err error
		key, err = ECDSAP256.NewPrivateKey()
		if err != nil {
			t.Fatalf("生成私钥失败: %v", err)
		}
	}
	if tmpl.SerialNumber == nil {
		tmpl.SerialNumber = big.NewInt(1)
	}
	if parent == nil {
		parent, parentKey = tmpl, key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, key.Public(), parentKey)
	if err != nil {
		t.Fatalf("签发测试证书失败: %v", err)
	}
	return der
}

// writeCertPair 在 dir 下写出 cert.pem/key.pem 形式的证书对。
func writeCertPair(t *testing.T, dir string, der []byte, key crypto.Signer) (string, string) {
	t.Helper()
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("解析测试证书失败: %v", err)
	}
	keyPEM, err := EncodeKeyPEM(key)
	if err != nil {
		t.Fatalf("编码测试私钥失败: %v", err)
	}
	certPath := filepath.Join(dir, "test.crt")
	keyPath := filepath.Join(dir, "test.key")
	mustWriteFile(t, certPath, EncodeCertPEM(cert), 0o644)
	mustWriteFile(t, keyPath, keyPEM, 0o600)
	return certPath, keyPath
}

// testCertTemplate 返回一张普通叶子证书模板。
func testCertTemplate(cn string, notBefore, notAfter time.Time) *x509.Certificate {
	return &x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
}

// caTemplate 返回一张 CA 模板。
func caTemplate(cn string, notBefore, notAfter time.Time) *x509.Certificate {
	return &x509.Certificate{
		SerialNumber:          big.NewInt(3),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
}

// generatedKey 生成一张自签证书对并返回路径与私钥。
func generatedKeyPair(t *testing.T, dir, cn string, notBefore, notAfter time.Time, isCA bool) (string, string, crypto.Signer) {
	t.Helper()
	key, err := ECDSAP256.NewPrivateKey()
	if err != nil {
		t.Fatalf("生成私钥失败: %v", err)
	}
	var tmpl *x509.Certificate
	if isCA {
		tmpl = caTemplate(cn, notBefore, notAfter)
	} else {
		tmpl = testCertTemplate(cn, notBefore, notAfter)
	}
	der := selfSigned(t, key, tmpl, nil, nil)
	certPath, keyPath := writeCertPair(t, dir, der, key)
	return certPath, keyPath, key
}

// pemBlock 把任意 DER 包成 PEM（测试里用来构造非法或非预期的 PEM 块）。
func pemBlock(typ string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
}

// tamper 翻转 PEM 正文的第一个 base64 字符，制造「被篡改的 PEM」。
func tamper(data []byte) []byte {
	out := append([]byte(nil), data...)
	start := bytes.IndexByte(out, '\n')
	if start < 0 || start+1 >= len(out) {
		return out
	}
	if c := out[start+1]; c >= 'a' && c <= 'z' {
		out[start+1] = c - 'a' + 'A'
	} else {
		out[start+1] = 'A'
	}
	return out
}
