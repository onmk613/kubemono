// Package encryption 管理 apiserver 的静态加密（encryption-at-rest）配置
// 关键属性：
// - 密钥只在 master 上生成并以 0600 保存，**绝不写入集群**（与 CA 同等对待）；
// - 生成一次、永久复用：重复执行 install 不会轮换密钥，否则已有 Secret 将无法解密；
// - 使用 secretbox（XSalsa20-Poly1305）算法，与 apiserver 的 built-in provider 对应。
package encryption

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"strings"

	"sigs.k8s.io/yaml"

	"kubemono/internal/util/fsutil"
)

// KeySize 是 apiserver 要求的 secretbox 密钥长度。
const KeySize = 32

// providerName 是记录在配置里的密钥名。
const providerName = "kubemono-key-1"

// Config 对应 apiserver 的 EncryptionConfiguration，仅覆盖 kubemono 渲染的子集。
type Config struct {
	APIVersion string     `json:"apiVersion"`
	Kind       string     `json:"kind"`
	Resources  []Resource `json:"resources"`
}

// Resource 是一组被加密的资源。
type Resource struct {
	Resources []string   `json:"resources"`
	Providers []Provider `json:"providers"`
}

// Provider 是一条加密 provider 条目。
type Provider struct {
	Secretbox *Secretbox `json:"secretbox,omitempty"`
	Identity  *struct{}  `json:"identity,omitempty"`
}

// Secretbox 承载 secretbox 密钥。
type Secretbox struct {
	Keys []Key `json:"keys"`
}

// Key 是一个具名密钥。
type Key struct {
	Name   string `json:"name"`
	Secret string `json:"secret"`
}

// GenerateKey 返回一个新的 secretbox 密钥。
func GenerateKey() ([]byte, error) {
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("生成静态加密密钥失败: %w", err)
	}
	return key, nil
}

// Render 为给定密钥构造 EncryptionConfiguration。
// 先 secretbox 后 identity：读用 secretbox，写用 secretbox，
// identity 只作为"无法解密时明文的兜底"。
func Render(key []byte) ([]byte, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("静态加密密钥长度必须是 %d 字节，实际 %d", KeySize, len(key))
	}
	cfg := Config{
		APIVersion: "apiserver.config.k8s.io/v1",
		Kind:       "EncryptionConfiguration",
		Resources: []Resource{
			{
				Resources: []string{"secrets", "configmaps"},
				Providers: []Provider{
					{Secretbox: &Secretbox{Keys: []Key{{
						Name:   providerName,
						Secret: base64.StdEncoding.EncodeToString(key),
					}}}},
					{Identity: &struct{}{}},
				},
			},
		},
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("序列化静态加密配置失败: %w", err)
	}
	return data, nil
}

// LoadKey 从已有配置文件中提取 secretbox 密钥。
func LoadKey(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := &Config{}
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", path, err)
	}
	return keyFromConfig(cfg)
}

// keyFromConfig 返回解析后配置中的第一个 secretbox 密钥。
func keyFromConfig(cfg *Config) ([]byte, error) {
	for _, res := range cfg.Resources {
		for _, p := range res.Providers {
			if p.Secretbox == nil {
				continue
			}
			for _, k := range p.Secretbox.Keys {
				raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(k.Secret))
				if err != nil {
					return nil, fmt.Errorf("密钥 %s 不是合法的 base64: %w", k.Name, err)
				}
				if len(raw) != KeySize {
					return nil, fmt.Errorf("密钥 %s 长度必须是 %d 字节，实际 %d", k.Name, KeySize, len(raw))
				}
				return raw, nil
			}
		}
	}
	return nil, fmt.Errorf("配置里没有 secretbox 密钥")
}

// EnsureConfig 确保 path 上存在静态加密配置。
// 返回值表示是否新写入（false 表示复用已有密钥，未改动任何内容）。
// allowCreate 只在首次 init 时为 true：已安装 master（或加入中的 master）缺失
// 该文件时必须失败，而不是生成新 key——新 key 会让已有 Secret 永久不可读。
// 安全约束：**绝不**因为“现有文件不可解析/缺 secretbox key”就生成新密钥覆盖。
func EnsureConfig(path string, allowCreate bool) (bool, error) {
	if fsutil.FileExists(path) {
		if _, err := LoadKey(path); err == nil {
			return false, nil
		} else {
			// 保留一份诊断副本，但绝不覆盖原文件。
			backup := path + ".invalid"
			if !fsutil.FileExists(backup) {
				if data, readErr := os.ReadFile(path); readErr == nil {
					if writeErr := fsutil.WriteFileAtomic(backup, data, 0o600); writeErr != nil {
						return false, fmt.Errorf("备份不可用的加密配置失败: %w", writeErr)
					}
				}
			}
			return false, fmt.Errorf(
				"现有静态加密配置 %s 不可用（%v）；为避免轮换密钥导致已有 Secret 永久无法解密，"+
					"kubemono 不会自动重建。请恢复该文件；确认集群没有任何加密数据时，可先移走 %s 再重试",
				path, err, path)
		}
	}
	if !allowCreate {
		return false, fmt.Errorf("静态加密配置 %s 不存在；已安装/加入中的 master 不会自动生成新密钥"+
			"（会让已有 Secret 永久无法解密）。请从备份或证书包恢复该文件；确认集群没有任何加密数据后，"+
			"在首次 init 时重新生成", path)
	}
	key, err := GenerateKey()
	if err != nil {
		return false, err
	}
	content, err := Render(key)
	if err != nil {
		return false, err
	}
	if err := fsutil.WriteFileAtomic(path, content, 0o600); err != nil {
		return false, fmt.Errorf("写入 %s 失败: %w", path, err)
	}
	return true, nil
}
