// Package runtime 管理节点容器运行时（containerd/custom），两者都安装 CNI 插件。
package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"kubemono/internal/bom"
	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	"kubemono/internal/render"
	"kubemono/internal/systemd"
	"kubemono/internal/util/fsutil"
	"kubemono/internal/util/output"
)

// ComponentContainerd 是 containerd 在 BOM、渲染结果与备份中的组件名。
const ComponentContainerd = "containerd"

// 的默认值：kubemono 托管的 containerd。
const (
	// DefaultCRISocket 是 kubemono 托管 containerd 的 CRI socket。
	DefaultCRISocket = "unix:///run/containerd/containerd.sock"
	// DefaultServiceName 是 kubemono 托管 containerd 的 systemd unit 名。
	DefaultServiceName = "containerd"
	// DefaultUnitName 是 containerd 的 systemd unit 文件名。
	DefaultUnitName = DefaultServiceName + ".service"
	// UnitPath 是 containerd 的 systemd unit 路径。
	UnitPath = "/etc/systemd/system/" + DefaultUnitName
)

// CommandResult 是一次外部命令的执行结果。
type CommandResult struct {
	// Stdout 是标准输出。
	Stdout []byte
	// Stderr 是标准错误。
	Stderr []byte
	// ExitCode 是退出码；-1 表示命令根本没有被启动。
	ExitCode int
}

// ErrCommandNotFound 表示可执行文件不存在或当前平台不支持执行命令。
// Status 依据它给出“ctr 未安装”这类可操作的提示。
var ErrCommandNotFound = errors.New("命令不存在")

// CommandRunner 执行外部命令；systemctl / ctr / tar 都经过它。
type CommandRunner interface {
	Run(ctx context.Context, name string, args ...string) (CommandResult, error)
}

// ServiceManager 是 systemctl 原语；实现与声明都来自 internal/systemd。
type ServiceManager = systemd.Actioner

// Status 是运行时的可观测状态。
type Status struct {
	// Running 表示 unit 处于 active。
	Running bool
	// CRISocket 是查询所用的 CRI socket。
	CRISocket string
	// ConfigHash 是磁盘上受管配置（config.toml + certs.d）的摘要；
	// custom 运行时永远为空（kubemono 不托管它的配置）。
	ConfigHash string
	// Message 是给操作者看的一句话状态。
	Message string
}

// Provider 是容器运行时的管理接口。
type Provider interface {
	// Name 返回运行时类型：containerd 或 custom。
	Name() string
	// Install 安装运行时所需的二进制（幂等）。
	Install(ctx context.Context) error
	// ApplyConfig 应用渲染结果并返回重启/热加载判定；custom 实现不写任何文件。
	ApplyConfig(ctx context.Context, spec *render.Spec) (*Plan, error)
	// Status 返回运行时状态；containerd 托管时用 ctr 确认 API 可用。
	Status(ctx context.Context) (Status, error)
	// Restart 重启运行时（custom 重启 serviceName）。
	Restart(ctx context.Context) error
}

// Options 是 Provider 的全部依赖与开关。
type Options struct {
	// Paths 解析所有目标路径（测试时指向临时目录）。
	Paths constants.Paths
	// BOM 提供运行时组件的版本（用于缺失检查与日志）。
	BOM *bom.BOM
	// Release 是当前 kubemono 版本，用于定位物料目录。
	Release string
	// InstallDir 是运行二进制的安装目录，空值取 /usr/local/bin。
	InstallDir string
	// DryRun 只报告，不写任何文件、不执行任何命令。
	DryRun bool
	// Report 接收进度消息。
	Report output.Reporter
	// Commands 执行外部命令；nil 使用当前平台的默认实现。
	Commands CommandRunner
	// Services 执行 systemd 动作；nil 使用当前平台的默认实现。
	Services ServiceManager
	// Current 覆盖“变更前”的受管配置（测试与 phase 复用）；
	// 为 nil 时从磁盘读取 config.toml + certs.d。
	Current *render.Spec
	// Now 返回当前时间（可注入）。
	Now func() time.Time
}

// withDefaults 返回补齐默认值后的副本。
func (o Options) withDefaults() Options {
	if o.InstallDir == "" {
		o.InstallDir = o.Paths.LocalBin()
	}
	if o.Report == nil {
		o.Report = output.Discard()
	}
	if o.Commands == nil {
		o.Commands = defaultCommandRunner()
	}
	if o.Services == nil {
		o.Services = defaultServiceManager()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

// 默认值只在 withDefaults 一处填充（NewProvider 已走它）；下面的访问器直读
// 字段，避免两边各自维护回退导致默认值漂移。

func (o Options) reporter() output.Reporter { return o.Report }

func (o Options) commands() CommandRunner { return o.Commands }

func (o Options) services() ServiceManager { return o.Services }

func (o Options) now() time.Time { return o.Now() }

func (o Options) installDir() string {
	if o.InstallDir != "" {
		return o.InstallDir
	}
	return o.Paths.LocalBin()
}

// artifactDir 返回本次 release 的物料目录。
func (o Options) artifactDir() string { return o.Paths.ArtifactDir(o.Release) }

// DefaultServiceManager 返回当前平台的默认服务管理器（linux 上是 systemd）。
func DefaultServiceManager() ServiceManager { return defaultServiceManager() }

// NewProvider 根据 node.runtime 的字段选择 containerd 或 custom 实现。
// custom 任一字段非空即启用 custom；否则默认 containerd。
func NewProvider(cfg clusterconfig.RuntimeSpec, opts Options) (Provider, error) {
	opts = opts.withDefaults()
	if cfg.IsCustom() {
		if cfg.Custom.CRISocket == "" {
			return nil, fmt.Errorf("node.runtime.custom.criSocket 不能为空")
		}
		if cfg.Custom.ServiceName == "" {
			return nil, fmt.Errorf("node.runtime.custom.serviceName 不能为空")
		}
		return &customProvider{cfg: cfg, opts: opts}, nil
	}
	return &containerdProvider{cfg: cfg, opts: opts}, nil
}

// CRISocket 返回该运行时使用的 CRI socket。
func CRISocket(cfg clusterconfig.RuntimeSpec) string {
	if cfg.IsCustom() {
		return cfg.Custom.CRISocket
	}
	return DefaultCRISocket
}

// ServiceName 返回该运行时的 systemd unit 名。
func ServiceName(cfg clusterconfig.RuntimeSpec) string {
	if cfg.IsCustom() {
		return cfg.Custom.ServiceName
	}
	return DefaultServiceName
}

// Binary 描述一个需要安装到 /usr/local/bin 的可执行文件。
type Binary struct {
	// Name 是可执行文件名，例如 containerd。
	Name string
	// Component 是 BOM 组件名，例如 containerd、runc、ctr。
	Component string
	// Category 是 BOM 类别，同时是物料目录 bin/<category>/… 的一级。
	Category string
	// Version 是该组件的 BOM 版本（用于缺失检查与日志，不决定目录）。
	Version string
	// Required 表示缺失时运行时不完整（Check 会判定为未就绪）。
	Required bool
}

// Binaries 返回运行时二进制；CNI 插件由 installCNIPlugins 单独安装。
func Binaries(cfg clusterconfig.RuntimeSpec, b *bom.BOM) []Binary {
	version := func(name string) string {
		if b == nil {
			return ""
		}
		return b.ComponentVersion(name)
	}
	out := make([]Binary, 0, 5)
	if !cfg.IsCustom() {
		out = append(out,
			Binary{Name: "containerd", Component: ComponentContainerd, Category: bom.CategoryRuntime, Version: version(ComponentContainerd), Required: true},
			Binary{Name: "containerd-shim-runc-v2", Component: ComponentContainerd, Category: bom.CategoryRuntime, Version: version(ComponentContainerd), Required: true},
			Binary{Name: "runc", Component: "runc", Category: bom.CategoryRuntime, Version: version("runc"), Required: true},
			// ctr 是 containerd 自带的客户端，默认随 containerd 安装。
			Binary{Name: "ctr", Component: ComponentContainerd, Category: bom.CategoryRuntime, Version: version(ComponentContainerd), Required: true},
		)
	}
	return out
}

// CNIPlugins 返回 BOM 声明的 CNI 插件名。
func CNIPlugins(b *bom.BOM) []string {
	if b == nil {
		return nil
	}
	c, ok := b.Components["cni-plugins"]
	if !ok {
		return nil
	}
	return append([]string(nil), c.Binaries...)
}

// LinkPath 返回 Binary 在安装目录中的实际路径。
func LinkPath(installDir, name string) string {
	if installDir == "" {
		installDir = constants.DefaultLocalBinDir
	}
	return filepath.Join(installDir, name)
}

// MissingBinaries 返回缺失的必需二进制。
func MissingBinaries(paths constants.Paths, installDir string, bins []Binary) []string {
	if installDir == "" {
		installDir = paths.LocalBin()
	}
	var out []string
	for _, b := range bins {
		if !b.Required {
			continue
		}
		if checkBinary(installDir, b) != nil {
			out = append(out, b.Name)
		}
	}
	return out
}

// MissingCNIPlugins 返回 /opt/cni/bin 中缺失或不可执行的插件。
func MissingCNIPlugins(paths constants.Paths, b *bom.BOM) []string {
	names := CNIPlugins(b)
	out := make([]string, 0, len(names))
	for _, name := range names {
		info, err := os.Stat(filepath.Join(paths.CNIBin(), name))
		if err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
			out = append(out, name)
		}
	}
	return out
}

// checkBinary 校验安装目录中的可执行文件。
func checkBinary(installDir string, b Binary) error {
	target := LinkPath(installDir, b.Name)
	info, err := os.Lstat(target)
	if err != nil {
		return fmt.Errorf("%s 未安装：%w", b.Name, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("%s 不是真实可执行文件（%s）", b.Name, target)
	}
	return nil
}

// installBinary 把源文件复制到安装目录（幂等）。
func installBinary(installDir string, b Binary, src string) error {
	return copyExecutable(src, LinkPath(installDir, b.Name))
}

// copyExecutable 内容不同才落盘，避免每次 install 都重写大文件。
// 复制本身走 fsutil.CopyFileStreaming：流式 + 原子写，几十 MiB 的容器运行时
// 二进制不必整体读进内存，失败也不会在 /usr/local/bin 留下半截文件。
func copyExecutable(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("读取物料 %s 失败：%w", src, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("物料 %s 不是普通文件", src)
	}
	// 目标位置若是软链，必须替换成真实文件；不能因为内容相同就跳过。
	if info, err := os.Lstat(dst); err == nil && info.Mode()&os.ModeSymlink == 0 && sameContent(src, dst) {
		return nil
	}
	mode := info.Mode().Perm() | 0o755
	if err := fsutil.CopyFileStreaming(src, dst, mode); err != nil {
		return fmt.Errorf("写入 %s 失败：%w", dst, err)
	}
	return nil
}

// sameContent 判断两个文件内容是否一致（先比大小，再比摘要）。
func sameContent(a, b string) bool {
	ai, err := os.Stat(a)
	if err != nil {
		return false
	}
	bi, err := os.Stat(b)
	if err != nil || ai.Size() != bi.Size() {
		return false
	}
	ah, err := fsutil.SHA256File(a)
	if err != nil {
		return false
	}
	bh, err := fsutil.SHA256File(b)
	if err != nil {
		return false
	}
	return ah == bh
}
