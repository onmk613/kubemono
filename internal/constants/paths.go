package constants

import (
	"path/filepath"
	"strings"
)

// Paths 在可选的测试根目录下解析所有 kubemono 已知路径。
// 生产环境 Root 为空，路径就是绝对路径；单元测试用 Root 把整套目录树搬到临时目录，
// CLI 不暴露这个 Root。KubemonoDir 覆盖 kubemono 自身数据目录（默认 /usr/local/kubemono）。
type Paths struct {
	Root string
	// KubemonoDir 为空时使用默认的 /usr/local/kubemono。
	KubemonoDir string
	// KubeletDir / EtcdDataDir 为空时使用默认目录。
	KubeletDir  string
	EtcdDataDir string
}

// NewPaths 返回使用给定 root 的 Paths（"" 表示真实文件系统）。
func NewPaths(root string) Paths { return Paths{Root: root} }

// WithKubemonoDir 覆盖 kubemono 自身数据目录（只影响 kubemono 根目录及其子目录）。
func (p Paths) WithKubemonoDir(dir string) Paths {
	p.KubemonoDir = strings.TrimSpace(dir)
	return p
}

// WithDataDirs 覆盖 kubelet 与 etcd 数据目录。
func (p Paths) WithDataDirs(kubeletDir, etcdDataDir string) Paths {
	p.KubeletDir = strings.TrimSpace(kubeletDir)
	p.EtcdDataDir = strings.TrimSpace(etcdDataDir)
	return p
}

// Resolve 把根前缀与绝对路径拼接起来。
func (p Paths) Resolve(path string) string {
	if p.Root == "" {
		return path
	}
	if path == "" {
		return p.Root
	}
	rel := path
	if filepath.IsAbs(rel) {
		rel = rel[1:]
	}
	return filepath.Join(p.Root, rel)
}

// Kubernetes 返回 Kubernetes 配置根目录；同组的 PKI 等路径同样遵循 Root 重定位。
func (p Paths) Kubernetes() string       { return p.Resolve(KubernetesDir) }
func (p Paths) PKI() string              { return p.Resolve(PKIDir) }
func (p Paths) EtcdPKI() string          { return p.Resolve(EtcdPKIDir) }
func (p Paths) Manifests() string        { return p.Resolve(ManifestsDir) }
func (p Paths) EncryptionConfig() string { return p.Resolve(EncryptionConfigDir) }
func (p Paths) Audit() string            { return p.Resolve(AuditDir) }
func (p Paths) Kubelet() string {
	if p.KubeletDir != "" {
		return p.Resolve(p.KubeletDir)
	}
	return p.Resolve(KubeletDir)
}
func (p Paths) KubeletConfig() string { return filepath.Join(p.Kubelet(), "config.yaml") }

// Kubemono 返回 kubemono 的唯一状态/配置根目录。
func (p Paths) Kubemono() string {
	dir := p.KubemonoDir
	if dir == "" {
		dir = KubemonoDir
	}
	return p.Resolve(dir)
}
func (p Paths) NodeConfig() string    { return filepath.Join(p.Kubemono(), "node.yaml") }
func (p Paths) ClusterConfig() string { return filepath.Join(p.Kubemono(), "cluster.yaml") }
func (p Paths) Desired() string       { return filepath.Join(p.Kubemono(), "desired") }
func (p Paths) DesiredChecksums() string {
	return filepath.Join(p.Desired(), "checksums.json")
}

func (p Paths) Artifacts() string { return filepath.Join(p.Kubemono(), "artifacts") }
func (p Paths) State() string     { return filepath.Join(p.Kubemono(), "state") }

// LocalBin 返回运行二进制的安装目录（/usr/local/bin）。
func (p Paths) LocalBin() string { return p.Resolve(DefaultLocalBinDir) }
func (p Paths) HostInitRecord() string {
	return filepath.Join(p.Kubemono(), "hostinit.json")
}
func (p Paths) Backup() string { return filepath.Join(p.Kubemono(), "backup") }

// CNIConfig 返回 CNI 配置目录；同组的 CNIBin/CNIData/EtcdData 同理。
func (p Paths) CNIConfig() string { return p.Resolve(CNIConfigDir) }
func (p Paths) CNIBin() string    { return p.Resolve(CNIBinDir) }
func (p Paths) CNIData() string   { return p.Resolve(CNIDataDir) }
func (p Paths) EtcdData() string {
	if p.EtcdDataDir != "" {
		return p.Resolve(p.EtcdDataDir)
	}
	return p.Resolve(EtcdDataDir)
}

// KubeconfigPath 返回 Kubernetes 配置目录下某个组件的 kubeconfig 路径。
func (p Paths) KubeconfigPath(name string) string {
	return filepath.Join(p.Kubernetes(), name)
}

// CertPath 返回 PKI 目录下某个证书或私钥的路径。
func (p Paths) CertPath(name string) string { return filepath.Join(p.PKI(), name) }

// ServiceAccountPub 返回 ServiceAccount 公钥的路径。
func (p Paths) ServiceAccountPub() string { return p.CertPath(ServiceAccountPubName) }

// ServiceAccountKey 返回 ServiceAccount 私钥的路径。
func (p Paths) ServiceAccountKey() string { return p.CertPath(ServiceAccountKeyName) }

// EtcdCertPath 返回 etcd PKI 目录下某个证书或私钥的路径。
func (p Paths) EtcdCertPath(name string) string {
	return filepath.Join(p.EtcdPKI(), name)
}

// ManifestPath 返回某个组件的静态 Pod 清单路径。
func (p Paths) ManifestPath(name string) string {
	return filepath.Join(p.Manifests(), name+".yaml")
}

// ArtifactDir 返回某个 release 的导入目录。
func (p Paths) ArtifactDir(release string) string {
	return filepath.Join(p.Artifacts(), release)
}

// DefaultPaths 返回生产（无前缀）路径。
func DefaultPaths() Paths { return Paths{} }
