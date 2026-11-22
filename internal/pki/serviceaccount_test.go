package pki

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateServiceAccountKeyPair(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pki", "nested")
	pubPath := filepath.Join(dir, "sa.pub")
	keyPath := filepath.Join(dir, "sa.key")

	if err := GenerateServiceAccountKeyPair(pubPath, keyPath); err != nil {
		t.Fatalf("GenerateServiceAccountKeyPair() 失败: %v", err)
	}
	assertMode(t, pubPath, 0o644)
	assertMode(t, keyPath, 0o600)
	assertMode(t, dir, 0o700)
	assertNoTempFiles(t, dir)

	signer, err := LoadServiceAccountKeyPair(pubPath, keyPath)
	if err != nil {
		t.Fatalf("LoadServiceAccountKeyPair() 失败: %v", err)
	}
	rsaKey, ok := signer.(*rsa.PrivateKey)
	if !ok {
		t.Fatalf("私钥类型 = %T, 期望 *rsa.PrivateKey", signer)
	}
	if bits := rsaKey.N.BitLen(); bits != ServiceAccountKeyBits {
		t.Fatalf("位数 = %d, 期望 %d", bits, ServiceAccountKeyBits)
	}

	// 私钥必须是 PKCS#8。
	keyPEM := mustRead(t, keyPath)
	block, _ := pem.Decode(keyPEM)
	if block == nil || block.Type != "PRIVATE KEY" {
		t.Fatalf("sa.key 应为 PKCS#8 PRIVATE KEY，实际 %+v", block)
	}
	if _, err := x509.ParsePKCS8PrivateKey(block.Bytes); err != nil {
		t.Fatalf("sa.key 不是合法 PKCS#8: %v", err)
	}
	// 公钥必须是 PKIX。
	pubPEM := mustRead(t, pubPath)
	if block, _ := pem.Decode(pubPEM); block == nil || block.Type != "PUBLIC KEY" {
		t.Fatalf("sa.pub 应为 PKIX PUBLIC KEY，实际 %+v", block)
	}

	// 覆盖生成：仍然可用且没有临时文件残留。
	if err := GenerateServiceAccountKeyPair(pubPath, keyPath); err != nil {
		t.Fatalf("覆盖生成失败: %v", err)
	}
	if _, err := LoadServiceAccountKeyPair(pubPath, keyPath); err != nil {
		t.Fatalf("覆盖后加载失败: %v", err)
	}
	assertNoTempFiles(t, dir)
}

func TestLoadServiceAccountKeyPair(t *testing.T) {
	dir := t.TempDir()
	pubPath := filepath.Join(dir, "sa.pub")
	keyPath := filepath.Join(dir, "sa.key")
	if err := GenerateServiceAccountKeyPair(pubPath, keyPath); err != nil {
		t.Fatal(err)
	}

	// 另一对密钥，用来制造不匹配。
	otherPub := filepath.Join(t.TempDir(), "sa.pub")
	otherKey := filepath.Join(t.TempDir(), "sa.key")
	if err := GenerateServiceAccountKeyPair(otherPub, otherKey); err != nil {
		t.Fatal(err)
	}

	// 一张 EC 私钥（apiserver 不接受）。
	ecDir := t.TempDir()
	ecKey, err := ECDSAP256.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	ecPEM, err := EncodeKeyPEM(ecKey)
	if err != nil {
		t.Fatal(err)
	}
	ecPath := filepath.Join(ecDir, "ec.key")
	mustWriteFile(t, ecPath, ecPEM, 0o600)

	tamperedKey := filepath.Join(t.TempDir(), "tampered.key")
	mustWriteFile(t, tamperedKey, tamper(mustRead(t, keyPath)), 0o600)
	tamperedPub := filepath.Join(t.TempDir(), "tampered.pub")
	mustWriteFile(t, tamperedPub, tamper(mustRead(t, pubPath)), 0o644)

	tests := []struct {
		name    string
		pub     string
		key     string
		wantErr error
		anyErr  bool
	}{
		{name: "合法", pub: pubPath, key: keyPath},
		{name: "公钥文件缺失", pub: filepath.Join(dir, "missing.pub"), key: keyPath, anyErr: true},
		{name: "私钥文件缺失", pub: pubPath, key: filepath.Join(dir, "missing.key"), anyErr: true},
		{name: "不是一对", pub: otherPub, key: keyPath, wantErr: ErrKeyMismatch},
		{name: "EC 私钥被拒绝", pub: pubPath, key: ecPath, anyErr: true},
		{name: "私钥被篡改", pub: pubPath, key: tamperedKey, anyErr: true},
		{name: "公钥被篡改", pub: tamperedPub, key: keyPath, anyErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			signer, err := LoadServiceAccountKeyPair(tt.pub, tt.key)
			if tt.anyErr {
				if err == nil {
					t.Fatal("期望错误")
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
				t.Fatalf("LoadServiceAccountKeyPair() 失败: %v", err)
			}
			if _, ok := signer.(*rsa.PrivateKey); !ok {
				t.Fatalf("私钥类型 = %T", signer)
			}
		})
	}

	if _, err := LoadServiceAccountKeyPair(pubPath, ecPath); err == nil || !strings.Contains(err.Error(), "RSA") {
		t.Fatalf("EC 私钥的错误信息应说明必须是 RSA: %v", err)
	}
}
