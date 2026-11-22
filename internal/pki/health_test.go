package pki

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// certFileCase 描述一个已落盘的证书文件。
type certFileCase struct {
	name     string
	certPath string
	keyPath  string
}

// newCertFileCase 生成证书/私钥文件；notAfter 为未来时间或过去时间决定健康状态。
func newCertFileCase(t *testing.T, cn string, notBefore, notAfter time.Duration, isCA bool) certFileCase {
	t.Helper()
	dir := t.TempDir()
	certPath, keyPath, _ := generatedKeyPair(t, dir, cn, time.Now().Add(notBefore), time.Now().Add(notAfter), isCA)
	return certFileCase{name: cn, certPath: certPath, keyPath: keyPath}
}

func TestCertificateExists(t *testing.T) {
	valid := newCertFileCase(t, "valid-leaf", -time.Hour, 365*24*time.Hour, false)
	expiring := newCertFileCase(t, "expiring-leaf", -time.Hour, 10*24*time.Hour, false)
	expired := newCertFileCase(t, "expired-leaf", -48*time.Hour, -time.Hour, false)
	other := newCertFileCase(t, "other", -time.Hour, 365*24*time.Hour, false)
	validCA := newCertFileCase(t, "valid-ca", -time.Hour, 10*365*24*time.Hour, true)

	tests := []struct {
		name     string
		certPath string
		keyPath  string
		want     bool
	}{
		{name: "有效证书对", certPath: valid.certPath, keyPath: valid.keyPath, want: true},
		{name: "有效 CA 对", certPath: validCA.certPath, keyPath: validCA.keyPath, want: true},
		{name: "证书缺失", certPath: filepath.Join(t.TempDir(), "x.crt"), keyPath: valid.keyPath},
		{name: "私钥缺失", certPath: valid.certPath, keyPath: filepath.Join(t.TempDir(), "x.key")},
		{name: "私钥与证书不匹配", certPath: valid.certPath, keyPath: other.keyPath},
		{name: "已过期", certPath: expired.certPath, keyPath: expired.keyPath},
		{name: "进入续期阈值", certPath: expiring.certPath, keyPath: expiring.keyPath},
		{name: "路径互换", certPath: valid.keyPath, keyPath: valid.certPath},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CertificateExists(tt.certPath, tt.keyPath); got != tt.want {
				t.Fatalf("CertificateExists() = %v, 期望 %v", got, tt.want)
			}
		})
	}
}

func TestFileExists(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	mustWriteFile(t, file, []byte("x"), 0o644)
	tests := []struct {
		name string
		path string
		want bool
	}{
		{name: "普通文件", path: file, want: true},
		{name: "目录", path: dir, want: false},
		{name: "不存在", path: filepath.Join(dir, "missing"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FileExists(tt.path); got != tt.want {
				t.Fatalf("FileExists() = %v, 期望 %v", got, tt.want)
			}
		})
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
