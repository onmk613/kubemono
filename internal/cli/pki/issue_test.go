package pki_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kubemono/internal/cli/clitest"
	pkipkg "kubemono/internal/pki"
)

// newTestCA 生成一对临时 CA 文件。
func newTestCA(t *testing.T) (string, string) {
	t.Helper()
	ca, err := pkipkg.NewCA("pki-cli-test-ca", pkipkg.ECDSAP256, 0)
	if err != nil {
		t.Fatalf("NewCA: %v", err)
	}
	dir := t.TempDir()
	certPath := filepath.Join(dir, "ca.crt")
	keyPath := filepath.Join(dir, "ca.key")
	if err := ca.WriteToDisk(certPath, keyPath); err != nil {
		t.Fatalf("WriteToDisk: %v", err)
	}
	return certPath, keyPath
}

// TestPKIIssuePeer 覆盖最小签发路径：peer、DNS/IP SAN、输出文件与权限。
func TestPKIIssuePeer(t *testing.T) {
	caCert, caKey := newTestCA(t)
	outDir := t.TempDir()
	out, err := clitest.ExecuteRoot(t,
		"pki", "issue",
		"--ca", caCert,
		"--ca-key", caKey,
		"--profile", "peer",
		"--cn", "etcd-1",
		"--san", "etcd-1.example.com",
		"--san", "127.0.0.1",
		"--days", "30",
		"--out-dir", outDir,
		"--output", "json",
	)
	if err != nil {
		t.Fatalf("pki issue: %v\n%s", err, out)
	}
	var res struct {
		CertPath   string `json:"certPath"`
		KeyPath    string `json:"keyPath"`
		CommonName string `json:"commonName"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("JSON: %v\n%s", err, out)
	}
	if res.CertPath != filepath.Join(outDir, "etcd-1.crt") || res.KeyPath != filepath.Join(outDir, "etcd-1.key") {
		t.Fatalf("paths = %q/%q", res.CertPath, res.KeyPath)
	}
	for path, mode := range map[string]os.FileMode{res.CertPath: 0o644, res.KeyPath: 0o600} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if info.Mode().Perm() != mode {
			t.Fatalf("%s mode = %04o，期望 %04o", path, info.Mode().Perm(), mode)
		}
	}
	infos, err := pkipkg.Inspect(res.CertPath)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if infos[0].Subject == "" || !strings.Contains(infos[0].Subject, "etcd-1") {
		t.Fatalf("subject = %q", infos[0].Subject)
	}
	if len(infos[0].DNSNames) != 1 || len(infos[0].IPs) != 1 {
		t.Fatalf("SANs = %+v", infos[0])
	}
}

// TestPKIIssueServerRequiresSAN 固定 server/peer 零 SAN 失败关闭。
func TestPKIIssueServerRequiresSAN(t *testing.T) {
	caCert, caKey := newTestCA(t)
	out, err := clitest.ExecuteRoot(t,
		"pki", "issue", "--ca", caCert, "--ca-key", caKey,
		"--profile", "server", "--cn", "server", "--out-dir", t.TempDir(),
	)
	if err == nil {
		t.Fatalf("server 零 SAN 必须失败:\n%s", out)
	}
	if !strings.Contains(err.Error(), "--san") {
		t.Fatalf("错误应提示 --san：%v", err)
	}
}

// TestPKIIssueRefusesOverwrite 固定不覆盖已有证书。
func TestPKIIssueRefusesOverwrite(t *testing.T) {
	caCert, caKey := newTestCA(t)
	outDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outDir, "client.crt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := clitest.ExecuteRoot(t,
		"pki", "issue", "--ca", caCert, "--ca-key", caKey,
		"--profile", "client", "--cn", "client", "--out-dir", outDir,
	)
	if err == nil {
		t.Fatalf("已有文件必须拒绝覆盖:\n%s", out)
	}
}
