package kubeconfig

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"kubemono/internal/pki"
)

func TestConfigUserNameAndContextName(t *testing.T) {
	tests := []struct {
		name        string
		user        string
		wantUser    string
		wantContext string
	}{
		{name: "显式用户", user: "kubernetes-admin", wantUser: "kubernetes-admin", wantContext: "kubernetes-admin@kubernetes"},
		{name: "默认用户", user: "", wantUser: DefaultUserName, wantContext: DefaultUserName + "@" + ClusterName},
		{name: "纯空格视为未指定", user: "   ", wantUser: DefaultUserName, wantContext: DefaultUserName + "@" + ClusterName},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{User: tt.user}
			if got := cfg.UserName(); got != tt.wantUser {
				t.Fatalf("UserName() = %q, 期望 %q", got, tt.wantUser)
			}
			if got := cfg.ContextName(); got != tt.wantContext {
				t.Fatalf("ContextName() = %q, 期望 %q", got, tt.wantContext)
			}
		})
	}
}

func TestBuild(t *testing.T) {
	tests := []struct {
		name  string
		cfg   Config
		check func(t *testing.T, got clientcmdapi.Config)
	}{
		{
			name: "证书身份",
			cfg: Config{
				User:           "kubernetes-admin",
				Server:         "https://127.0.0.1:6443",
				CACertPath:     "/etc/kubernetes/pki/ca.crt",
				ClientCertPath: "/etc/kubernetes/pki/admin.crt",
				ClientKeyPath:  "/etc/kubernetes/pki/admin.key",
			},
			check: func(t *testing.T, got clientcmdapi.Config) {
				if got.CurrentContext != "kubernetes-admin@kubernetes" {
					t.Fatalf("current-context = %q", got.CurrentContext)
				}
				cluster := got.Clusters[ClusterName]
				if cluster == nil || cluster.Server != "https://127.0.0.1:6443" || cluster.CertificateAuthority != "/etc/kubernetes/pki/ca.crt" {
					t.Fatalf("cluster = %+v", cluster)
				}
				user := got.AuthInfos["kubernetes-admin"]
				if user == nil || user.ClientCertificate != "/etc/kubernetes/pki/admin.crt" || user.ClientKey != "/etc/kubernetes/pki/admin.key" {
					t.Fatalf("user = %+v", user)
				}
				ctx := got.Contexts[got.CurrentContext]
				if ctx == nil || ctx.Cluster != ClusterName || ctx.AuthInfo != "kubernetes-admin" {
					t.Fatalf("context = %+v", ctx)
				}
			},
		},
		{
			name: "token 身份且未指定用户",
			cfg:  Config{Server: "https://127.0.0.1:6443", Token: "abcdef.0123456789abcdef"},
			check: func(t *testing.T, got clientcmdapi.Config) {
				if got.CurrentContext != DefaultUserName+"@"+ClusterName {
					t.Fatalf("current-context = %q", got.CurrentContext)
				}
				user := got.AuthInfos[DefaultUserName]
				if user == nil || user.Token != "abcdef.0123456789abcdef" {
					t.Fatalf("user = %+v", user)
				}
				if user.ClientCertificate != "" || user.ClientKey != "" {
					t.Fatal("token 身份不应带客户端证书")
				}
			},
		},
		{
			name: "最小配置",
			cfg:  Config{Server: "https://10.0.0.1:6443"},
			check: func(t *testing.T, got clientcmdapi.Config) {
				if len(got.Clusters) != 1 || len(got.AuthInfos) != 1 || len(got.Contexts) != 1 {
					t.Fatalf("条目数 = %d/%d/%d", len(got.Clusters), len(got.AuthInfos), len(got.Contexts))
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Build(tt.cfg)
			tt.check(t, got)
		})
	}
}

func TestWriteLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admin.conf")
	original := Build(Config{
		User:           "kubernetes-admin",
		Server:         "https://127.0.0.1:6443",
		CACertPath:     "/etc/kubernetes/pki/ca.crt",
		ClientCertPath: "/etc/kubernetes/pki/admin.crt",
		ClientKeyPath:  "/etc/kubernetes/pki/admin.key",
	})
	if err := WriteToFile(path, original); err != nil {
		t.Fatalf("WriteToFile() 失败: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load() 失败: %v", err)
	}
	if loaded.CurrentContext != original.CurrentContext {
		t.Fatalf("current-context = %q, 期望 %q", loaded.CurrentContext, original.CurrentContext)
	}
	// clientcmd 解码后会把空的 Extensions 变成空 map，比较前先归一化。
	if !reflect.DeepEqual(normalizeConfig(loaded).Clusters, normalizeConfig(original).Clusters) {
		t.Fatalf("clusters = %+v", loaded.Clusters)
	}
	if !reflect.DeepEqual(normalizeConfig(loaded).AuthInfos, normalizeConfig(original).AuthInfos) {
		t.Fatalf("users = %+v", loaded.AuthInfos)
	}
	if !reflect.DeepEqual(normalizeConfig(loaded).Contexts, normalizeConfig(original).Contexts) {
		t.Fatalf("contexts = %+v", loaded.Contexts)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"apiVersion: v1", "kind: Config", "server: https://127.0.0.1:6443", "name: kubernetes-admin"} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("文件内容缺少 %q:\n%s", want, data)
		}
	}
}

func TestWriteToFileModeAndDirs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pki", "kubeconfig", "kubelet.conf")
	if err := WriteToFile(path, Build(Config{User: "system:node:n1", Server: "https://127.0.0.1:6443"})); err != nil {
		t.Fatal(err)
	}
	assertFileMode(t, path, 0o600)
	assertDirMode(t, filepath.Dir(path), 0o755)

	// 覆盖一个权限过宽的文件后仍然必须是 0600（rename 语义）。
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteToFile(path, Build(Config{User: "system:node:n1", Server: "https://127.0.0.1:6443"})); err != nil {
		t.Fatal(err)
	}
	assertFileMode(t, path, 0o600)

	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Fatalf("残留临时文件 %s", e.Name())
		}
	}
}

func TestLoadErrors(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.conf")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	garbage := filepath.Join(dir, "garbage.conf")
	if err := os.WriteFile(garbage, []byte("这不是 kubeconfig: ["), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		path    string
		wantErr string
	}{
		{name: "文件不存在", path: filepath.Join(dir, "missing.conf"), wantErr: "读取"},
		{name: "空文件", path: empty, wantErr: "解析"},
		{name: "非法内容", path: garbage, wantErr: "解析"},
		{name: "目录", path: dir, wantErr: "读取"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Load(tt.path); err == nil {
				t.Fatal("期望错误")
			} else if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("错误信息 %q 应包含 %q", err, tt.wantErr)
			}
		})
	}
}

// normalizeConfig 把 clientcmd 解码产生的空 map 归一化成 nil，便于比较。
func normalizeConfig(cfg clientcmdapi.Config) clientcmdapi.Config {
	for _, c := range cfg.Clusters {
		if c != nil && len(c.Extensions) == 0 {
			c.Extensions = nil
		}
	}
	for _, u := range cfg.AuthInfos {
		if u != nil && len(u.Extensions) == 0 {
			u.Extensions = nil
		}
	}
	for _, c := range cfg.Contexts {
		if c != nil && len(c.Extensions) == 0 {
			c.Extensions = nil
		}
	}
	if len(cfg.Preferences.Extensions) == 0 {
		cfg.Preferences.Extensions = nil
	}
	return cfg
}

// TestDeterministicOutput 保证同一份配置反复渲染得到完全相同的字节，
// 否则 desired 校验和会每次都变化。
func TestDeterministicOutput(t *testing.T) {
	cfg := Build(Config{
		User:           "kubernetes-admin",
		Server:         "https://127.0.0.1:6443",
		CACertPath:     "/etc/kubernetes/pki/ca.crt",
		ClientCertPath: "/etc/kubernetes/pki/admin.crt",
		ClientKeyPath:  "/etc/kubernetes/pki/admin.key",
	})
	multi := clientcmdapi.Config{
		Preferences: clientcmdapi.Preferences{},
		Clusters: map[string]*clientcmdapi.Cluster{
			"z": {Server: "https://z:6443"},
			"a": {Server: "https://a:6443"},
			"m": {Server: "https://m:6443"},
		},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{
			"z": {Token: "z"},
			"a": {Token: "a"},
			"m": {Token: "m"},
		},
		Contexts: map[string]*clientcmdapi.Context{
			"z": {Cluster: "z", AuthInfo: "z"},
			"a": {Cluster: "a", AuthInfo: "a"},
			"m": {Cluster: "m", AuthInfo: "m"},
		},
		CurrentContext: "a",
	}
	dir := t.TempDir()
	for name, config := range map[string]clientcmdapi.Config{"single": cfg, "multi": multi} {
		t.Run(name, func(t *testing.T) {
			first, err := clientcmd.Write(config)
			if err != nil {
				t.Fatal(err)
			}
			second, err := clientcmd.Write(config)
			if err != nil {
				t.Fatal(err)
			}
			if string(first) != string(second) {
				t.Fatalf("两次序列化结果不同:\n%s\n---\n%s", first, second)
			}
			pathA := filepath.Join(dir, name+"-a.conf")
			pathB := filepath.Join(dir, name+"-b.conf")
			if err := WriteToFile(pathA, config); err != nil {
				t.Fatal(err)
			}
			if err := WriteToFile(pathB, config); err != nil {
				t.Fatal(err)
			}
			dataA, err := os.ReadFile(pathA)
			if err != nil {
				t.Fatal(err)
			}
			dataB, err := os.ReadFile(pathB)
			if err != nil {
				t.Fatal(err)
			}
			if string(dataA) != string(dataB) {
				t.Fatalf("两次写盘结果不同:\n%s\n---\n%s", dataA, dataB)
			}
		})
	}
}

// TestGroupOEncoding 覆盖「分组写进客户端证书 O」的路径：
// kubeconfig 不携带分组成员，分组必须由证书 Subject 的 O 表达。
func TestGroupOEncoding(t *testing.T) {
	cfg := Config{
		User:           "kubernetes-admin",
		Server:         "https://127.0.0.1:6443",
		ClientCertPath: "/c.crt",
		ClientKeyPath:  "/c.key",
	}
	built := Build(cfg)
	if len(built.AuthInfos) != 1 {
		t.Fatalf("users = %+v", built.AuthInfos)
	}
	for name, user := range built.AuthInfos {
		if len(user.ImpersonateGroups) != 0 || user.Impersonate != "" {
			t.Fatalf("用户 %q 不应使用模拟身份: %+v", name, user)
		}
	}
	data, err := clientcmd.Write(built)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "act-as") {
		t.Fatalf("kubeconfig 不应包含 act-as:\n%s", data)
	}

	ca, err := pki.NewCA("kubernetes-ca", pki.ECDSAP256, 10*365*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cn, orgs := cfg.UserName(), []string{GroupClusterAdminsForTest}
	cert, _, err := ca.Sign(pki.CertConfig{CommonName: cn, Organizations: orgs, Usages: pki.UsageClient})
	if err != nil {
		t.Fatal(err)
	}
	if cert.Subject.CommonName != "kubernetes-admin" {
		t.Fatalf("CN = %q", cert.Subject.CommonName)
	}
	if len(cert.Subject.Organization) != 1 || cert.Subject.Organization[0] != GroupClusterAdminsForTest {
		t.Fatalf("O = %v, 期望 [%s]", cert.Subject.Organization, GroupClusterAdminsForTest)
	}
}

const GroupClusterAdminsForTest = "kubemono:cluster-admins"

func assertFileMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s 失败: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s 权限 = %o, 期望 %o", path, got, want)
	}
}

func assertDirMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s 失败: %v", path, err)
	}
	if !info.IsDir() {
		t.Fatalf("%s 不是目录", path)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s 权限 = %o, 期望 %o", path, got, want)
	}
}
