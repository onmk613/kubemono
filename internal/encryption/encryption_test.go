package encryption

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestRenderAndLoadKeyRoundTrip(t *testing.T) {
	key, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if len(key) != KeySize {
		t.Fatalf("key length = %d", len(key))
	}
	data, err := Render(key)
	if err != nil {
		t.Fatal(err)
	}
	// 必须是合法的 EncryptionConfiguration。
	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		t.Fatalf("渲染结果不是合法 YAML: %v", err)
	}
	if cfg.Kind != "EncryptionConfiguration" || cfg.APIVersion != "apiserver.config.k8s.io/v1" {
		t.Fatalf("apiVersion/kind 不正确: %s/%s", cfg.APIVersion, cfg.Kind)
	}
	if len(cfg.Resources) != 1 || len(cfg.Resources[0].Providers) != 2 {
		t.Fatalf("providers 结构不正确: %+v", cfg.Resources)
	}
	if cfg.Resources[0].Providers[0].Secretbox == nil || cfg.Resources[0].Providers[1].Identity == nil {
		t.Fatal("provider 顺序必须是 secretbox → identity")
	}
	res := strings.Join(cfg.Resources[0].Resources, ",")
	if !strings.Contains(res, "secrets") || !strings.Contains(res, "configmaps") {
		t.Fatalf("加密的资源范围不正确: %s", res)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadKey(path)
	if err != nil {
		t.Fatalf("LoadKey: %v", err)
	}
	if string(loaded) != string(key) {
		t.Fatal("读回的密钥与生成的不一致")
	}
}

func TestRenderRejectsWrongKeySize(t *testing.T) {
	if _, err := Render([]byte("short")); err == nil {
		t.Fatal("错误的密钥长度必须报错")
	}
}

func TestEnsureConfigIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "encryption", "config.yaml")

	created, err := EnsureConfig(path, true)
	if err != nil {
		t.Fatalf("EnsureConfig: %v", err)
	}
	if !created {
		t.Fatal("首次调用应该创建配置")
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("密钥文件权限必须是 0600，实际 %04o", info.Mode().Perm())
	}
	if !strings.Contains(string(first), "EncryptionConfiguration") {
		t.Fatal("配置缺少 apiVersion/kind")
	}

	// 再次调用必须复用同一把密钥（否则已有 Secret 无法解密）。
	created, err = EnsureConfig(path, true)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("已有合法配置时不应重建")
	}
	second, _ := os.ReadFile(path)
	if string(first) != string(second) {
		t.Fatal("重复调用改变了加密配置（密钥被轮换）")
	}
}

// TestEnsureConfigRefusesCorruptFile: 现有配置不可用时必须失败，而不是生成新密钥
// 覆盖——否则已有 Secret 会永久无法解密。原文件与诊断副本都必须保留。
func TestEnsureConfigRefusesCorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	corrupt := []byte("this: is: not: valid")
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}
	created, err := EnsureConfig(path, true)
	if err == nil {
		t.Fatal("损坏的配置必须报错，不能静默轮换密钥")
	}
	if created {
		t.Fatal("报错路径不能报告 created=true")
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(after) != string(corrupt) {
		t.Fatal("原配置必须保持不变（不能被新密钥覆盖）")
	}
	// 原文件必须留有诊断副本。
	if _, err := os.Stat(path + ".invalid"); err != nil {
		t.Fatalf("损坏文件应被备份: %v", err)
	}
	if _, err := LoadKey(path); err == nil {
		t.Fatal("原配置仍应不可用（未被覆盖）")
	}
}

// 一个“语法合法但没有 secretbox key”的配置同样不能被自动轮换。
func TestEnsureConfigRefusesConfigWithoutSecretboxKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := []byte("apiVersion: apiserver.config.k8s.io/v1\nkind: EncryptionConfiguration\nresources:\n- resources: [secrets]\n  providers:\n  - identity: {}\n")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureConfig(path, true); err == nil {
		t.Fatal("没有 secretbox 密钥的现有配置必须报错，不能生成新密钥")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(content) {
		t.Fatal("现有配置未被修改")
	}
}

func TestLoadKeyRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("apiVersion: v1\nkind: EncryptionConfiguration\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKey(path); err == nil {
		t.Fatal("没有 secretbox 密钥时必须报错")
	}
	if _, err := LoadKey(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Fatal("文件不存在必须报错")
	}
}

// TestEnsureConfigRefusesMissingWhenNotInit: 已安装/加入中的 master 缺失配置
// 时必须 fail-closed，绝不能生成新 key。
func TestEnsureConfigRefusesMissingWhenNotInit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "encryption", "config.yaml")
	created, err := EnsureConfig(path, false)
	if err == nil {
		t.Fatal("非 init 场景缺失加密配置必须报错")
	}
	if created {
		t.Fatal("报错路径不能报告 created=true")
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Fatal("失败时不能写出新配置")
	}
}
