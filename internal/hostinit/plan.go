package hostinit

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strings"

	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
)

// Step 的动作类型（Step.Kind）。
const (
	KindSysctl = "sysctl"
	KindModule = "module"
	KindFile   = "file"
	KindDir    = "dir"
	KindHook   = "hook"
)

// Step 是初始化计划中的一步。
// Step 是纯数据（可直接 reflect.DeepEqual 比较、序列化、diff），
// 真正的主机改动由 ApplyWithOptions(ctx, steps, applier, journal, opts) 完成。
type Step struct {
	// ID 在计划内唯一且稳定，是幂等与 diff 的依据。
	ID string
	// Title 是给用户看的中文描述。
	Title string
	// Kind 见 Kind* 常量。
	Kind string
	// Payload 是该步骤的 typed 参数（见 SysctlPayload / FilePayload 等）。
	Payload any
}

// Applier 是主机变更的执行器。
// 所有实现都必须可注入，因此本包在 darwin 上也能测试。
// 可选能力通过 FileReader / DirMaker / SysctlReader 扩展接口提供；
// 缺少可选能力时 Apply 只对需要它的步骤报错。
//
// 所有方法都接收调用方的 ctx：使用 context.Background() 会让
// Ctrl-C 之后的 modprobe / hook 命令照跑到底；纯文件操作无法感知 ctx
// （os/fsutil 无 ctx 变体），但统一签名保证将来接入时不用再改接口。
type Applier interface {
	// WriteFile 写入文件（父目录必须已存在或由 MakeDir 创建）。
	WriteFile(ctx context.Context, path string, content []byte, mode fs.FileMode) error
	// RemoveFile 删除文件或空目录。
	RemoveFile(ctx context.Context, path string) error
	// Sysctl 设置内核参数。
	Sysctl(ctx context.Context, key, value string) error
	// LoadModule 加载内核模块。
	LoadModule(ctx context.Context, name string) error
	// RunCommand 执行外部命令（目前只用于 preHostInit hook）。
	RunCommand(ctx context.Context, name string, args ...string) error
}

// FileReader 是 Applier 的可选扩展：读取现有文件内容。
// /etc/hosts 的受管区块、fstab、limits.d、sysctl.d 等“先读后写”的步骤
// 以及精确还原都依赖它。
type FileReader interface {
	ReadFile(ctx context.Context, path string) ([]byte, error)
}

// DirMaker 是 Applier 的可选扩展：递归创建目录。
type DirMaker interface {
	MakeDir(ctx context.Context, path string, mode fs.FileMode) error
}

// SysctlReader 是 Applier 的可选扩展：读取内核参数当前值。
type SysctlReader interface {
	ReadSysctl(ctx context.Context, key string) (string, error)
}

// 重启后需要由 systemd 重放的持久化文件。
const (
	modulesLoadPath = "/etc/modules-load.d/kubemono.conf"
	sysctlConfPath  = "/etc/sysctl.d/99-kubemono.conf"
)

// Plan 依据集群配置、本机节点与主机事实生成确定性计划。
// 步骤顺序固定：用户钩子 → 内核模块 → sysctl → 目录。
// 内核模块与 sysctl 的完整清单来自 cluster.yaml，没有隐藏默认值或 extra 追加。
// 同一输入必然产生完全相同的输出（reflect.DeepEqual 相等）。
func Plan(cfg *clusterconfig.Cluster, host *clusterconfig.Host) ([]Step, error) {
	if cfg == nil {
		return nil, errors.New("hostinit: 集群配置不能为 nil")
	}
	if host == nil {
		return nil, errors.New("hostinit: 本机节点不能为 nil")
	}
	steps := make([]Step, 0, 16)
	steps = append(steps, planHooks(cfg)...)
	steps = append(steps, planModules(cfg)...)
	steps = append(steps, planSysctls(cfg)...)
	steps = append(steps, planDirs(cfg, host)...)
	return steps, nil
}

// planModules 生成内核模块步骤。完整清单来自 cluster.yaml；重启后 modprobe
// 不会自动重放，因此列表同步写进 modules-load.d。空列表也会写空文件，
// 保证上一次的受管列表被清掉。
func planModules(cfg *clusterconfig.Cluster) []Step {
	modules := normalizeModules(cfg.Node.Host.KernelModules)
	steps := make([]Step, 0, len(modules)+1)
	for _, name := range modules {
		steps = append(steps, Step{
			ID:      "module-" + name,
			Title:   "加载内核模块 " + name,
			Kind:    KindModule,
			Payload: ModulePayload{Name: name},
		})
	}
	content := ""
	if len(modules) > 0 {
		content = strings.Join(modules, "\n") + "\n"
	}
	steps = append(steps, Step{
		ID:    "modules-load",
		Title: "持久化内核模块列表到 /etc/modules-load.d/kubemono.conf",
		Kind:  KindFile,
		Payload: FilePayload{
			Path:    modulesLoadPath,
			Mode:    0o644,
			Content: content,
		},
	})
	return steps
}

// planSysctls 生成 sysctl 步骤。完整清单来自 cluster.yaml；运行时值不会跨重启
// 保留，因此同步写 /etc/sysctl.d 让 systemd-sysctl 重放。键名排序保证顺序确定，
// 空 map 也会写空文件以清掉上一次的受管内容。
func planSysctls(cfg *clusterconfig.Cluster) []Step {
	sysctls := cfg.Node.Host.Sysctls
	keys := slices.Sorted(maps.Keys(sysctls))
	steps := make([]Step, 0, len(keys)+1)
	for _, key := range keys {
		steps = append(steps, Step{
			ID:      "sysctl-" + key,
			Title:   "设置内核参数 " + key + "=" + sysctls[key],
			Kind:    KindSysctl,
			Payload: SysctlPayload{Key: key, Value: sysctls[key]},
		})
	}
	var b strings.Builder
	for _, key := range keys {
		fmt.Fprintf(&b, "%s = %s\n", key, sysctls[key])
	}
	steps = append(steps, Step{
		ID:    "sysctl-conf",
		Title: "持久化内核参数到 /etc/sysctl.d/99-kubemono.conf",
		Kind:  KindFile,
		Payload: FilePayload{
			Path:    sysctlConfPath,
			Mode:    0o644,
			Content: b.String(),
		},
	})
	return steps
}

// normalizeModules 去掉空白项与重复项，保持配置里的声明顺序。
func normalizeModules(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, m := range in {
		name := strings.TrimSpace(m)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// planDirs 生成目录步骤，按路径排序保证顺序确定。
func planDirs(cfg *clusterconfig.Cluster, host *clusterconfig.Host) []Step {
	dirs := section32Dirs(cfg, host)
	steps := make([]Step, 0, len(dirs))
	for _, dir := range slices.Sorted(maps.Keys(dirs)) {
		steps = append(steps, Step{
			ID:    "dir-" + dir,
			Title: "创建目录 " + dir,
			Kind:  KindDir,
			Payload: DirPayload{
				Path: dir,
				Mode: dirs[dir],
			},
		})
	}
	return steps
}

// planHooks 生成 preHostInit 钩子步骤（保持配置里的声明顺序）。
func planHooks(cfg *clusterconfig.Cluster) []Step {
	steps := make([]Step, 0, len(cfg.Cluster.Hooks.PreHostInit))
	for i, hook := range cfg.Cluster.Hooks.PreHostInit {
		command := strings.TrimSpace(hook)
		if command == "" {
			continue
		}
		steps = append(steps, Step{
			ID:      fmt.Sprintf("hook-preHostInit-%d", i),
			Title:   "执行 preHostInit 钩子 " + command,
			Kind:    KindHook,
			Payload: HookPayload{Command: command},
		})
	}
	return steps
}

// renderFile 计算文件步骤的目标内容与变更前状态。
func renderFile(ctx context.Context, p FilePayload, ap Applier) (content, before string, revertible bool, err error) {
	reader, hasReader := ap.(FileReader)
	before = EntryAbsent
	if hasReader {
		data, rerr := reader.ReadFile(ctx, p.Path)
		switch {
		case rerr == nil:
			before = string(data)
		case errors.Is(rerr, fs.ErrNotExist):
			// 不存在：保留 EntryAbsent 哨兵。
		default:
			return "", "", false, rerr
		}
	}
	// 没有 FileReader 时无法知道原内容，记录降级为不可还原。
	return p.Content, before, hasReader, nil
}

// section32Dirs 返回 Kubernetes 必需目录及其权限位。
func section32Dirs(cfg *clusterconfig.Cluster, host *clusterconfig.Host) map[string]fs.FileMode {
	kubeletDir := constants.KubeletDir
	if dir := strings.TrimSpace(cfg.Node.Kubelet.RootDir); dir != "" {
		kubeletDir = dir
	}
	dirs := map[string]fs.FileMode{
		constants.KubernetesDir:       0o755,
		constants.PKIDir:              0o700,
		constants.ManifestsDir:        0o755,
		constants.TmpDir:              0o755,
		constants.EncryptionConfigDir: 0o700,
		constants.AuditDir:            0o755,
		kubeletDir:                    0o755,
		constants.KubemonoDir:         0o755,
		constants.DesiredDir:          0o755,
		constants.StateDir:            0o755,
		constants.ArtifactsDir:        0o755,
		constants.BackupDir:           0o755,
		constants.CNIConfigDir:        0o755,
		constants.CNIBinDir:           0o755,
		constants.CNIDataDir:          0o755,
	}
	if root := strings.TrimSpace(cfg.Node.Runtime.Containerd.Root); root != "" {
		dirs[root] = 0o755
	}
	// etcd 的 PKI 与数据目录只属于真正运行本地 etcd 的 master。纯 worker 与
	// external etcd 都不该出现空的 /etc/kubernetes/pki/etcd——空目录会让人
	// 误以为本机参与 etcd，也会混进 cert check-expiration 的扫描面。
	if host.IsMaster() && !cfg.ControlPlane.Etcd.IsExternal() {
		dirs[constants.EtcdPKIDir] = 0o700
		if dataDir := strings.TrimSpace(cfg.ControlPlane.Etcd.Local.DataDir); dataDir != "" {
			dirs[dataDir] = 0o700
		}
	}
	return dirs
}

// readSysctlValue 读取 sysctl 当前值（需要 Applier 实现 SysctlReader）。
func readSysctlValue(ctx context.Context, ap Applier, key string) (string, bool) {
	reader, ok := ap.(SysctlReader)
	if !ok {
		return EntryAbsent, false
	}
	value, err := reader.ReadSysctl(ctx, key)
	if err != nil || strings.TrimSpace(value) == "" {
		return EntryAbsent, false
	}
	return value, true
}
