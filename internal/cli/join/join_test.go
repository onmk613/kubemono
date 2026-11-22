package join

import (
	"encoding/hex"
	"strings"
	"testing"

	"kubemono/internal/cli/options"
	clusterconfig "kubemono/internal/config"
	"kubemono/internal/join/certs"
)

func TestNewRegistersJoinFlags(t *testing.T) {
	cmd := New(options.Default())
	if cmd.Use != "join [api-server-endpoint]" {
		t.Fatalf("Use = %q", cmd.Use)
	}
	for _, name := range []string{
		"config", "node", "allow-address-mismatch", "replace-pki", "dry-run", "wait",
		"token", "discovery-token-ca-cert-hash", "discovery-token-unsafe-skip-ca-verification",
		"control-plane", "certificate-key",
	} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("join 缺少 flag --%s", name)
		}
	}
}

func TestBuildJoinSecretFromCommandLine(t *testing.T) {
	hash := "sha256:" + strings.Repeat("0", 64)
	worker, err := buildJoinSecret("abcdef.0123456789abcdef", "10.0.0.1:6443", []string{hash}, false, false, "")
	if err != nil {
		t.Fatalf("worker: %v", err)
	}
	if worker.Role != clusterconfig.RoleWorker || worker.CertificateKey != "" {
		t.Fatalf("worker join secret = %+v", worker)
	}
	if len(worker.DiscoveryEndpoints) != 1 || worker.DiscoveryEndpoints[0] != "10.0.0.1:6443" {
		t.Fatalf("endpoint = %v", worker.DiscoveryEndpoints)
	}
	if len(worker.CACertHashes) != 1 || worker.CACertHashes[0] != hash {
		t.Fatalf("hashes = %v", worker.CACertHashes)
	}

	key := hex.EncodeToString(make([]byte, certs.KeySize))
	master, err := buildJoinSecret("abcdef.0123456789abcdef", "", []string{hash}, false, true, key)
	if err != nil {
		t.Fatalf("master: %v", err)
	}
	if master.Role != clusterconfig.RoleMaster {
		t.Fatalf("master role = %s", master.Role)
	}
	if _, err := certs.DecodeKey(master.CertificateKey); err != nil {
		t.Fatalf("hex certificate-key 未转成 base64: %v", err)
	}

	for _, tc := range []struct {
		name  string
		token string
		hash  []string
		skip  bool
		cp    bool
		key   string
	}{
		{"no hash", "abcdef.0123456789abcdef", nil, false, false, ""},
		{"master no key", "abcdef.0123456789abcdef", []string{hash}, false, true, ""},
		{"worker with key", "abcdef.0123456789abcdef", []string{hash}, false, false, key},
		{"bad token", "not-a-token", []string{hash}, false, false, ""},
	} {
		if _, err := buildJoinSecret(tc.token, "", tc.hash, tc.skip, tc.cp, tc.key); err == nil {
			t.Errorf("%s 必须被拒绝", tc.name)
		}
	}
	if _, err := buildJoinSecret("abcdef.0123456789abcdef", "http://10.0.0.1:6443", []string{hash}, false, false, ""); err == nil {
		t.Error("http:// 端点必须被拒绝")
	}
}
