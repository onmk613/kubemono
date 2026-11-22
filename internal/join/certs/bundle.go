// Package certs 实现 master 加入所需的证书包：从签发节点（master）的本机读取
// CA/SA 物料，用 certificateKey 加密后写入 kube-system/kubemono-certs，
// 供持同一把 key 的 master 加入者下载。
// 加入者用 bootstrap token 读 Secret，用 --certificate-key
// 解密即可，不需要应答方进程或一次性请求。
package certs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	"kubemono/internal/pki"
	"kubemono/internal/util/fsutil"
)

// Version 是证书包的结构版本；文件集合或语义变化时必须递增。
const Version = 1

// 证书包中的规范文件键（相对 /etc/kubernetes）。
const (
	// FileCACert 是集群根 CA 证书。
	FileCACert = "pki/ca.crt"
	// FileCAKey 是集群根 CA 私钥。
	FileCAKey = "pki/ca.key"
	// FileFrontProxyCACert 是聚合层 CA 证书。
	FileFrontProxyCACert = "pki/front-proxy-ca.crt"
	// FileFrontProxyCAKey 是聚合层 CA 私钥。
	FileFrontProxyCAKey = "pki/front-proxy-ca.key"
	// FileEtcdCACert 是堆叠 etcd 的 CA 证书。
	FileEtcdCACert = "pki/etcd/ca.crt"
	// FileEtcdCAKey 是堆叠 etcd 的 CA 私钥。
	FileEtcdCAKey = "pki/etcd/ca.key"
	// FileServiceAccountPub 是 ServiceAccount 公钥。
	FileServiceAccountPub = "pki/sa.pub"
	// FileServiceAccountKey 是 ServiceAccount 私钥。
	FileServiceAccountKey = "pki/sa.key"
	// FileEncryptionConfig 是静态加密配置。
	FileEncryptionConfig = "encryption/config.yaml"
)

// bundleFile 是一张证书包文件的规范：键、权限、是否必需/可选。
type bundleFile struct {
	Key      string
	Mode     fs.FileMode
	Required bool
	Optional bool
}

// bundleFiles 是版本 1 的唯一文件表：证书部分来自 pki.Catalogue，
// SA/加密配置由本包补充。
func bundleFiles() []bundleFile {
	var out []bundleFile
	for _, f := range pki.DefaultCatalogue.ShippedFiles() {
		out = append(out, bundleFile{Key: f.Key, Mode: f.Mode, Required: f.Required, Optional: f.Optional})
	}
	out = append(out,
		bundleFile{FileServiceAccountPub, pki.CertFileMode, true, false},
		bundleFile{FileServiceAccountKey, pki.KeyFileMode, true, false},
		bundleFile{FileEncryptionConfig, 0o600, true, false},
	)
	return out
}

var (
	requiredFiles = []string{}
	optionalFiles = []string{}
	knownFiles    = map[string]bool{}
	fileModes     = map[string]fs.FileMode{}
)

func init() {
	for _, f := range bundleFiles() {
		knownFiles[f.Key] = true
		fileModes[f.Key] = f.Mode
		if f.Required {
			requiredFiles = append(requiredFiles, f.Key)
		}
		if f.Optional {
			optionalFiles = append(optionalFiles, f.Key)
		}
	}
}

// 错误。
var (
	// ErrMalformed 表示证书包结构非法。
	ErrMalformed = errors.New("join-certs: 证书包格式非法")
	// ErrUnknownFile 表示证书包包含未知文件键。
	ErrUnknownFile = errors.New("join-certs: 未知文件键")
	// ErrCAKeyConflict 表示本机已有不同的 CA 私钥且未加 --replace-pki。
	ErrCAKeyConflict = errors.New("join-certs: CA 私钥冲突")
)

// Bundle 是加密前/解密后的证书包。
type Bundle struct {
	// Version 是结构版本。
	Version int `json:"version"`
	// ClusterID 是集群身份，用于提示与排障（不参与鉴权）。
	ClusterID string `json:"clusterID,omitempty"`
	// Files 是规范文件键到内容的映射。
	Files map[string][]byte `json:"files"`
}

// New 返回一个空证书包。
func New(clusterID string) *Bundle {
	return &Bundle{Version: Version, ClusterID: clusterID, Files: map[string][]byte{}}
}

// Validate 校验版本、文件键与必需文件集合。
func (b *Bundle) Validate() error {
	if b == nil {
		return fmt.Errorf("%w: 证书包为 nil", ErrMalformed)
	}
	if b.Version != Version {
		return fmt.Errorf("%w: 版本 %d，期望 %d", ErrMalformed, b.Version, Version)
	}
	if len(b.Files) == 0 {
		return fmt.Errorf("%w: 没有任何文件", ErrMalformed)
	}
	for key := range b.Files {
		if !knownFiles[key] {
			return fmt.Errorf("%w: %s", ErrUnknownFile, key)
		}
	}
	for _, key := range requiredFiles {
		if _, ok := b.Files[key]; !ok {
			return fmt.Errorf("%w: 缺少必需文件 %s", ErrMalformed, key)
		}
	}
	return nil
}

// TargetPath 把规范文件键映射到本机路径。
func TargetPath(paths constants.Paths, key string) (string, error) {
	switch {
	case key == FileEncryptionConfig:
		return filepath.Join(paths.EncryptionConfig(), constants.EncryptionConfigName), nil
	case strings.HasPrefix(key, "pki/etcd/"):
		name := strings.TrimPrefix(key, "pki/etcd/")
		if name == "" {
			return "", fmt.Errorf("%w: %s 缺少文件名", ErrUnknownFile, key)
		}
		return paths.EtcdCertPath(name), nil
	case strings.HasPrefix(key, "pki/"):
		name := strings.TrimPrefix(key, "pki/")
		if name == "" {
			return "", fmt.Errorf("%w: %s 缺少文件名", ErrUnknownFile, key)
		}
		return paths.CertPath(name), nil
	default:
		return "", fmt.Errorf("%w: %s 不是 pki/ 或 encryption/ 下的规范路径", ErrUnknownFile, key)
	}
}

// FileMode 返回规范文件键的落盘权限。
func FileMode(key string) (fs.FileMode, bool) {
	mode, ok := fileModes[key]
	return mode, ok
}

// Build 从签发节点本机读取密钥物料，按集群拓扑组装证书包。
// 堆叠 etcd 带 etcd CA；external 带
// apiserver 客户端证书、etcd CA 与可选的 kubemono 客户端证书；启用静态加密时
// 带上加密配置。
func Build(paths constants.Paths, cluster *clusterconfig.Cluster, clusterID string) (*Bundle, error) {
	if cluster == nil {
		return nil, fmt.Errorf("join-certs: 集群配置为空")
	}
	bundle := New(clusterID)
	required := append([]string(nil), requiredFiles...)
	if cluster.ControlPlane.Etcd.IsExternal() {
		for _, key := range optionalFiles {
			if err := bundle.addFromDisk(paths, key, false); err != nil {
				return nil, err
			}
		}
	} else {
		etcdCA := pki.DefaultCatalogue.MustAuthority(pki.CAEtcd)
		required = append(required, etcdCA.Store.FileKey(etcdCA.CertFile), etcdCA.Store.FileKey(etcdCA.KeyFile))
	}
	required = append(required, FileEncryptionConfig)
	for _, key := range required {
		if err := bundle.addFromDisk(paths, key, true); err != nil {
			return nil, err
		}
	}
	if err := bundle.Validate(); err != nil {
		return nil, err
	}
	return bundle, nil
}

// addFromDisk 读取一个文件键到证书包；required=false 时允许不存在。
func (b *Bundle) addFromDisk(paths constants.Paths, key string, required bool) error {
	target, err := TargetPath(paths, key)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(target)
	if err != nil {
		if !required && errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("读取 %s 失败: %w", target, err)
	}
	if len(data) == 0 {
		return fmt.Errorf("%s 内容为空", target)
	}
	b.Files[key] = data
	return nil
}

// WriteTo 把证书包写入本机；CA 私钥冲突时要求 force。
func (b *Bundle) WriteTo(paths constants.Paths, force bool, report func(string, ...any)) error {
	if err := b.Validate(); err != nil {
		return err
	}
	if report == nil {
		report = func(string, ...any) {}
	}
	keys := make([]string, 0, len(b.Files))
	for key := range b.Files {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	// 先做全量冲突预检，再写任何文件。
	// 证书包按 key 排序写盘，encryption/config.yaml、ca.crt、sa.key 都排在
	// ca.key 之前；如果只在处理 ca.key 时才报冲突，用户看到的是一条“失败”
	// 命令，但本机密钥材料已经被不可逆改写了。
	if !force {
		for _, key := range keys {
			if !conflictSensitive(key) {
				continue
			}
			target, err := TargetPath(paths, key)
			if err != nil {
				return err
			}
			existing, err := os.ReadFile(target)
			switch {
			case err == nil && string(existing) == string(b.Files[key]):
				continue
			case err == nil:
				return fmt.Errorf("%w: %s 已存在且与集群物料不同；覆盖会让已签发的证书或已加密的 Secret 失效。"+
					"确认要重建本机 PKI/加密密钥时加 --replace-pki，或先执行 kubemono reset", ErrCAKeyConflict, target)
			case errors.Is(err, fs.ErrNotExist):
				continue
			default:
				return fmt.Errorf("检查已有文件 %s 失败: %w", target, err)
			}
		}
	}
	for _, key := range keys {
		content := b.Files[key]
		mode, ok := FileMode(key)
		if !ok {
			return fmt.Errorf("%w: %s 没有定义落盘权限", ErrUnknownFile, key)
		}
		target, err := TargetPath(paths, key)
		if err != nil {
			return err
		}
		if isCAPrivateKey(key) {
			existing, err := os.ReadFile(target)
			switch {
			case err == nil && string(existing) == string(content):
				report("已有相同的 CA 私钥，跳过 %s", target)
				continue
			case err == nil && !force:
				// 预检已经拦过；保留这里作为防御（调用方可能绕过预检）。
				return fmt.Errorf("%w: %s 已存在且与集群 CA 不同；覆盖会让已签发的证书失效。"+
					"确认要重建本机 PKI 时加 --replace-pki，或先执行 kubemono reset", ErrCAKeyConflict, target)
			case err != nil && !errors.Is(err, fs.ErrNotExist):
				return fmt.Errorf("检查已有 CA 私钥 %s 失败: %w", target, err)
			}
		}
		changed, err := fsutil.WriteFileIfChanged(target, content, mode)
		if err != nil {
			return fmt.Errorf("写入 %s 失败: %w", target, err)
		}
		if changed {
			report("已写入 %s", target)
		}
	}
	return nil
}

// conflictSensitive 列出“内容不同就不能静默覆盖”的文件：
// CA 私钥、ServiceAccount 密钥对、静态加密配置。它们一旦被替换，
// 已签发证书或已加密 Secret 会不可逆失效。
func conflictSensitive(key string) bool {
	if isCAPrivateKey(key) {
		return true
	}
	switch key {
	case FileServiceAccountKey, FileServiceAccountPub, FileEncryptionConfig:
		return true
	default:
		return false
	}
}

func isCAPrivateKey(key string) bool {
	for _, a := range pki.DefaultCatalogue.Authorities {
		if key == a.Store.FileKey(a.KeyFile) {
			return true
		}
	}
	return false
}
