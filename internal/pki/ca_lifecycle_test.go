package pki

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// CA 进入叶子证书的 30 天续期窗口时，CAExists 必须仍为 true；
// 只有 CertificateExists（叶子语义）会返回 false。否则 certs phase 会重建 CA。
func TestCAExistsIgnoresLeafRenewalWindow(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")
	ca, err := NewCA("kubernetes-ca", RSA2048, 48*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := ca.WriteToDisk(certPath, keyPath); err != nil {
		t.Fatal(err)
	}
	if CertificateExists(certPath, keyPath) {
		t.Fatal("CertificateExists 对 48h 到期的 CA 应返回 false（叶子续期窗口）")
	}
	if !CAExists(certPath, keyPath) {
		t.Fatal("CAExists 对仍可用的 CA 应返回 true，不能套用叶子续期窗口")
	}
	loaded, present, err := LoadCAIfExists(certPath, keyPath)
	if err != nil || !present || loaded == nil {
		t.Fatalf("LoadCAIfExists = (%v, %v, %v), want loaded CA", loaded, present, err)
	}
}

func TestLoadCAIfExistsIncompleteAndMissing(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")

	if ca, present, err := LoadCAIfExists(certPath, keyPath); ca != nil || present || err != nil {
		t.Fatalf("两个文件都不存在时应返回 (nil,false,nil)，实际 (%v,%v,%v)", ca, present, err)
	}

	ca, err := NewCA("kubernetes-ca", RSA2048, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := ca.WriteToDisk(certPath, keyPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	if _, present, err := LoadCAIfExists(certPath, keyPath); !present || err == nil {
		t.Fatalf("只有证书、没有私钥时必须 present=true 且报错，实际 present=%v err=%v", present, err)
	}
	if CAExists(certPath, keyPath) {
		t.Fatal("不完整的 CA 材料不能报告为可用")
	}
}

// 叶子证书有效期不得超过 CA；否则 CA 到期后叶子证书仍然“看起来有效”，
// 组件会在 CA 过期后突然全部拒绝。
func TestIssueRejectsLeafOutlivingCA(t *testing.T) {
	ca, err := NewCA("kubernetes-ca", RSA2048, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ca.Sign(CertConfig{
		CommonName: "kube-apiserver",
		Validity:   2 * time.Hour,
	}); err == nil {
		t.Fatal("有效期 2h 超过 CA 的 1h，Sign 必须报错")
	}
	if _, _, err := ca.Sign(CertConfig{
		CommonName: "kube-apiserver",
		Validity:   30 * time.Minute,
	}); err != nil {
		t.Fatalf("30m 在 CA 有效期内，应成功: %v", err)
	}
}

func TestLoadCertificate(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")
	ca, err := NewCA("kubernetes-ca", ECDSAP256, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := ca.WriteToDisk(certPath, keyPath); err != nil {
		t.Fatal(err)
	}
	got, err := LoadCertificate(certPath)
	if err != nil {
		t.Fatal(err)
	}
	if !got.IsCA || got.Subject.CommonName != "kubernetes-ca" {
		t.Fatalf("LoadCertificate 返回了错误的证书: %+v", got.Subject)
	}
}
