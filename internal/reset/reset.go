package reset

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"kubemono/internal/bom"
	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	"kubemono/internal/execx"
	"kubemono/internal/systemd"
	"kubemono/internal/util/fsutil"
	"kubemono/internal/util/output"
)

// Step 是 的五个步骤之一。
type Step string

// 的五步（顺序即执行顺序，不可调整）。
const (
	// StepControlPlane：停止并禁用控制面 unit。
	StepControlPlane Step = "control-plane"
	// StepKubelet：停止 kubelet、用 ctr 停止容器、卸载 kubelet 数据目录下的挂载点。
	StepKubelet Step = "kubelet"
	// StepNetwork：原生网络清理。
	StepNetwork Step = "network"
	// StepData：备份 PKI 后删除配置与数据目录。
	StepData Step = "data"
	// StepRuntime：还原主机初始化（--purge 时最后删除 kubemono 根目录）。
	StepRuntime Step = "runtime"
)

// StepResult 是单个步骤的执行结果。
type StepResult struct {
	Step   Step   `json:"step"`
	Done   bool   `json:"done"`
	DryRun bool   `json:"dryRun,omitempty"`
	Error  string `json:"error,omitempty"`
}

// Deps 是 reset 的全部可注入依赖。
// 未设置的字段由 DefaultDeps 补齐（linux 是真实实现，其他平台报错）。
type Deps struct {
	// Runner 执行 systemctl / ctr / ipset。
	Runner execx.Runner
	// Host 是文件系统与挂载点端口。
	Host HostFS
	// Network 是原生网络清理器。
	Network NetworkCleaner
	// HostInit 还原主机初始化改动。
	HostInit HostInitReverter
}

// DefaultDeps 返回当前平台的默认依赖。
func DefaultDeps() Deps {
	runner := execx.Default()
	return Deps{
		Runner:   runner,
		Host:     NewHostFS(),
		Network:  NewNetworkCleaner(),
		HostInit: newHostInitReverter(),
	}
}

// Options 是 Reset 的全部开关。
type Options struct {
	// Paths 解析所有目标路径。
	Paths constants.Paths
	// RuntimeType 是运行时的类型（containerd|custom），决定是否按 containerd 的方式停服务、重建 CNI 目录。
	RuntimeType string
	// KeepData 保留 etcd 与 kubelet 数据目录（CLI：--keep-data）。
	KeepData bool
	// KeepHostInit 不还原主机初始化改动（CLI：--keep-hostinit）。
	KeepHostInit bool
	// Purge 连运行二进制、CNI 插件、containerd 数据/配置与 /usr/local/kubemono 一起删除。
	// CLI：--purge；与 KeepData/KeepHostInit 互斥。
	Purge bool
	// ContainerdDataDir 是 containerd 数据目录（--purge 时删除）；空值用默认值。
	ContainerdDataDir string
	// RuntimeUnit 是运行时 systemd unit（--purge 时停止/删除）；空值用 containerd.service。
	RuntimeUnit string
	// DryRun 只输出将要删除的清单，不执行任何会改主机的动作。
	DryRun bool
	// Report 接收进度消息。
	Report output.Reporter
	// Now 可注入（备份目录名用它的时间戳）。
	Now func() time.Time
	// Deps 可注入依赖；零值字段用平台默认实现补齐。
	Deps Deps
}

func (o *Options) withDefaults() *Options {
	cp := *o
	if cp.RuntimeType == "" {
		cp.RuntimeType = clusterconfig.RuntimeContainerd
	}
	if cp.Report == nil {
		cp.Report = output.Discard()
	}
	if cp.Now == nil {
		cp.Now = time.Now
	}
	if cp.Purge {
		// --purge 的语义就是全部清理，keep 开关不参与。
		cp.KeepData = false
		cp.KeepHostInit = false
	}
	if cp.ContainerdDataDir == "" && cp.RuntimeType != clusterconfig.RuntimeCustom {
		cp.ContainerdDataDir = "/var/lib/containerd"
	}
	if cp.RuntimeUnit == "" {
		cp.RuntimeUnit = RuntimeUnitName
	}
	defaults := DefaultDeps()
	if cp.Deps.Runner == nil {
		cp.Deps.Runner = defaults.Runner
	}
	if cp.Deps.Host == nil {
		cp.Deps.Host = defaults.Host
	}
	if cp.Deps.Network == nil {
		cp.Deps.Network = defaults.Network
	}
	if cp.Deps.HostInit == nil {
		cp.Deps.HostInit = defaults.HostInit
	}
	return &cp
}

// Result 是 reset 的执行结果。
// 它是 dry-run 与真实执行的统一输出：DryRun 时 Deleted 为空、WouldDelete 是
// 完整清单；真实执行时反过来。
type Result struct {
	// DryRun 表示本次是演练。
	DryRun bool `json:"dryRun,omitempty"`
	// Purge 表示本次包含二进制/CNI/运行时配置与数据目录的彻底清理。
	Purge bool `json:"purge,omitempty"`
	// BackupBase 是本次 PKI 备份根目录（<kubemono-root>/backup）。
	BackupBase string `json:"backupBase,omitempty"`
	// Steps 是五个步骤的执行结果。
	Steps []StepResult `json:"steps"`
	// BackupDir 是本次 PKI 备份目录（/usr/local/kubemono/backup/<ts>）。
	BackupDir string `json:"backupDir,omitempty"`
	// Deleted 是实际删除的路径。
	Deleted []string `json:"deleted,omitempty"`
	// WouldDelete 是 dry-run 下"将会删除"的路径清单。
	WouldDelete []string `json:"wouldDelete,omitempty"`
	// Skipped 是因开关（--keep-data/--keep-hostinit）而跳过的动作。
	Skipped []string `json:"skipped,omitempty"`
	// Network 是网络清理清单（dry-run 与真实执行都填）。
	Network NetworkTargets `json:"network"`
	// NetworkErrors 是未清理成功的网络项（不阻塞数据删除，但会返回错误）。
	NetworkErrors []string `json:"networkErrors,omitempty"`
	// Warnings 是需要人工留意的提示（例如主机初始化无法完全还原）。
	Warnings []string `json:"warnings,omitempty"`
}

// Run 执行 的五步重置。
// 语义：
// - 任何一步失败都会立即返回（含已删除内容的清单），不会继续往下删；
// - 第 4 步的 PKI 备份失败时绝不删除任何东西；
// - 网络清理失败不阻塞数据删除，但会在最后作为错误返回，退出码非零。
func Run(ctx context.Context, opts *Options) (*Result, error) {
	o := opts.withDefaults()
	res := &Result{DryRun: o.DryRun, Purge: o.Purge, Network: DefaultNetworkTargets(o.Paths)}

	steps := []struct {
		name Step
		fn   func(context.Context, *Options, *Result) error
	}{
		{StepControlPlane, stepControlPlane},
		{StepKubelet, stepKubelet},
		{StepNetwork, stepNetwork},
		{StepData, stepData},
		{StepRuntime, stepRuntime},
	}
	for _, step := range steps {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		err := step.fn(ctx, o, res)
		res.Steps = append(res.Steps, StepResult{
			Step:   step.name,
			Done:   err == nil,
			DryRun: o.DryRun,
			Error:  errorString(err),
		})
		if err != nil {
			return res, fmt.Errorf("reset 第 %d 步 %s 失败: %w", len(res.Steps), step.name, err)
		}
	}
	if len(res.NetworkErrors) > 0 {
		return res, fmt.Errorf("reset 已完成，但网络清理未完全成功：%s", strings.Join(res.NetworkErrors, "；"))
	}
	return res, nil
}

// stepControlPlane 让控制面停下来。
func stepControlPlane(ctx context.Context, o *Options, res *Result) error {
	units := systemd.UnitController{Runner: o.Deps.Runner, Report: o.Report, DryRun: o.DryRun}
	for _, unit := range ControlPlaneUnits {
		if err := units.Stop(ctx, unit); err != nil {
			return err
		}
		if err := units.Disable(ctx, unit); err != nil {
			return err
		}
	}
	return nil
}

// stepKubelet 停 kubelet、停全部 Pod、卸载 kubelet 目录下的挂载点。
func stepKubelet(ctx context.Context, o *Options, res *Result) error {
	if o.DryRun {
		o.Report.Infof("将停止 %s", constants.ComponentKubelet+".service")
		o.Report.Infof("将用 ctr 停止 k8s.io namespace 下的全部容器")
		o.Report.Infof("将卸载 %s 下的挂载点", o.Paths.Kubelet())
		return nil
	}
	units := systemd.UnitController{Runner: o.Deps.Runner, Report: o.Report}
	if err := units.Stop(ctx, constants.ComponentKubelet+".service"); err != nil {
		return err
	}
	if o.Purge {
		if err := units.Disable(ctx, constants.ComponentKubelet+".service"); err != nil {
			return err
		}
	}
	pods := podStopper{Runner: o.Deps.Runner, Report: o.Report}
	if _, err := pods.StopAll(ctx); err != nil {
		// kubelet 已停；Pod 停不干净不应该阻止后面的清理，但必须吵。
		o.Report.Warnf("停止 Pod 未完全成功：%v", err)
		res.Warnings = append(res.Warnings, fmt.Sprintf("停止 Pod 未完全成功：%v", err))
	}
	mounts, err := o.Deps.Host.ListMounts(ctx, o.Paths.Kubelet())
	if err != nil {
		return err
	}
	for _, mount := range mounts {
		if err := o.Deps.Host.Unmount(ctx, mount); err != nil {
			return err
		}
		o.Report.Infof("已卸载 %s", mount)
	}
	return nil
}

// stepNetwork 用原生实现清理 kubemono 创建的网络状态。
func stepNetwork(ctx context.Context, o *Options, res *Result) error {
	res.Network = DefaultNetworkTargets(o.Paths)
	if o.DryRun {
		o.Report.Infof("将清理以下网络状态（原生实现，不依赖组件二进制）：")
		for _, line := range res.Network.Describe() {
			o.Report.Infof("  - %s", line)
		}
		return nil
	}
	// 容器已在 stepKubelet 里用 ctr 停掉；这里直接清理网络，
	// 避免 kube-router 把刚删掉的 kubemono0 网桥/ipset 又建回来。

	// 默认保留运行时：containerd 监视 /etc/cni/net.d，目录被删时会以
	// "cni conf dir is removed, stop watching" fatal 退出；先停 containerd，
	// 清理后再把空的 CNI 目录建回来，避免 203/EXEC 或 cni monitor 崩溃。
	// --purge 要删运行时本身，停完不重建任何目录。
	if !o.DryRun && (o.RuntimeType == clusterconfig.RuntimeContainerd || o.Purge) {
		units := systemd.UnitController{Runner: o.Deps.Runner, Report: o.Report}
		if err := units.Stop(ctx, o.RuntimeUnit); err != nil {
			o.Report.Warnf("停止 %s 失败，网络清理可能让它进入重启循环：%v", o.RuntimeUnit, err)
			res.Warnings = append(res.Warnings, fmt.Sprintf("停止 %s 失败：%v", o.RuntimeUnit, err))
		}
		if o.Purge {
			if err := units.Disable(ctx, o.RuntimeUnit); err != nil {
				o.Report.Warnf("禁用 %s 失败：%v", o.RuntimeUnit, err)
				res.Warnings = append(res.Warnings, fmt.Sprintf("禁用 %s 失败：%v", o.RuntimeUnit, err))
			}
		}
	}

	if err := o.Deps.Network.CleanNetwork(ctx, res.Network); err != nil {
		res.NetworkErrors = append(res.NetworkErrors, err.Error())
		o.Report.Warnf("网络清理未完全成功：%v", err)
		return nil
	}
	// 保留运行时的情况下，CNI 目录必须存在：containerd 启动时会读取它，
	// 目录被删除会直接 fatal；空目录只会让 CRI 暂时没有可用 CNI。
	if !o.DryRun && o.RuntimeType == clusterconfig.RuntimeContainerd && !o.Purge {
		for _, dir := range []string{o.Paths.CNIConfig(), o.Paths.CNIData()} {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				o.Report.Warnf("重建 CNI 目录 %s 失败：%v", dir, err)
				res.Warnings = append(res.Warnings, fmt.Sprintf("重建 CNI 目录 %s 失败：%v", dir, err))
			}
		}
	}
	o.Report.Infof("网络清理完成")
	return nil
}

// stepData 备份 PKI 与静态加密配置后删除配置与数据目录。
// encryption/config.yaml 是 Secret 的解密密钥：只备份 PKI 不够，尤其在
// --keep-data 保留 etcd WAL 时，丢了它等于永久丢失已加密 Secret。
func stepData(ctx context.Context, o *Options, res *Result) error {
	backupBase := o.Paths.Backup()
	res.BackupBase = backupBase
	backupRoot := filepath.Join(backupBase, fsutil.Timestamp(o.Now()))
	pkiBackup := filepath.Join(backupRoot, "pki")
	encryptionBackup := filepath.Join(backupRoot, "encryption")
	encryptionDir := o.Paths.EncryptionConfig()
	targets := deletionList(o)
	for _, path := range keptDataPaths(o) {
		res.Skipped = append(res.Skipped, "保留数据目录 "+path+"（--keep-data）")
	}

	if o.DryRun {
		o.Report.Infof("将把 %s 备份到 %s", o.Paths.PKI(), pkiBackup)
		if o.Deps.Host.Exists(encryptionDir) {
			o.Report.Infof("将把 %s 备份到 %s", encryptionDir, encryptionBackup)
		}
		res.WouldDelete = append(res.WouldDelete, targets...)
		o.Report.Infof("将删除以下 %d 项：", len(targets))
		for _, path := range targets {
			o.Report.Infof("  - %s", path)
		}
		return nil
	}

	// 备份失败绝不允许继续删除。
	if o.Deps.Host.Exists(o.Paths.PKI()) {
		if err := o.Deps.Host.CopyTree(ctx, o.Paths.PKI(), pkiBackup); err != nil {
			return fmt.Errorf("备份 PKI 到 %s 失败，已中止删除: %w", pkiBackup, err)
		}
		res.BackupDir = backupRoot
		o.Report.Infof("已备份 PKI 到 %s", pkiBackup)
	} else {
		res.Warnings = append(res.Warnings, fmt.Sprintf("PKI 目录 %s 不存在，跳过备份", o.Paths.PKI()))
		o.Report.Warnf("PKI 目录 %s 不存在，跳过备份", o.Paths.PKI())
	}
	if o.Deps.Host.Exists(encryptionDir) {
		if err := o.Deps.Host.CopyTree(ctx, encryptionDir, encryptionBackup); err != nil {
			return fmt.Errorf("备份静态加密配置到 %s 失败，已中止删除: %w", encryptionBackup, err)
		}
		res.BackupDir = backupRoot
		o.Report.Infof("已备份静态加密配置到 %s", encryptionBackup)
	} else {
		o.Report.Debugf("静态加密配置目录 %s 不存在，跳过备份", encryptionDir)
	}

	for _, path := range targets {
		if o.Purge && path == o.Paths.Kubemono() {
			// hostinit.json 还在里面；等第 5 步还原完主机改动再删。
			o.Report.Debugf("%s 延迟到 host-init 还原后删除", path)
			continue
		}
		if !o.Deps.Host.Exists(path) {
			o.Report.Debugf("%s 不存在，跳过", path)
			continue
		}
		if err := o.Deps.Host.RemoveAll(ctx, path); err != nil {
			return err
		}
		res.Deleted = append(res.Deleted, path)
		o.Report.Infof("已删除 %s", path)
	}
	// 删除指向本机 admin.conf 的 ~/.kube/config 软链；用户自己的 kubeconfig
	// 不会被删除。
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		link := filepath.Join(home, ".kube", "config")
		if target, err := os.Readlink(link); err == nil &&
			filepath.Base(target) == constants.AdminKubeconfigName &&
			strings.Contains(filepath.ToSlash(target), "/etc/kubernetes/") {
			if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
				o.Report.Warnf("删除 %s 失败：%v", link, err)
			} else {
				o.Report.Infof("已删除 %s（指向 admin.conf 的软链）", link)
			}
		}
	}
	// 备份只保留最近若干份：退役节点/长期 reset 不应无限堆积旧 CA/加密材料。
	if err := fsutil.PruneTimestampedDirs(o.Paths.Backup(), fsutil.DefaultBackupKeep); err != nil {
		res.Warnings = append(res.Warnings, fmt.Sprintf("清理旧备份失败：%v", err))
		o.Report.Warnf("清理旧备份失败：%v", err)
	}
	// unit 文件消失后刷新 systemd，避免残留的 failed 状态。
	if o.Deps.Runner != nil {
		if _, err := o.Deps.Runner.Run(ctx, "systemctl", "daemon-reload"); err != nil {
			o.Report.Warnf("systemctl daemon-reload 失败：%v", err)
		}
	}
	return nil
}

// stepRuntime 还原主机初始化（运行时二进制默认保留）。
func stepRuntime(ctx context.Context, o *Options, res *Result) error {
	if o.KeepHostInit {
		res.Skipped = append(res.Skipped, "保留主机初始化改动（--keep-hostinit）")
		o.Report.Infof("保留主机初始化改动（--keep-hostinit）")
		return nil
	}
	if o.DryRun {
		o.Report.Infof("将按 %s 还原主机初始化改动", o.Paths.HostInitRecord())
		return nil
	}
	if err := o.Deps.HostInit.Revert(ctx, o.Paths); err != nil {
		// 不可自动还原的项必须让运维人员看到，但不改变"文件已删除"的事实。
		res.Warnings = append(res.Warnings, fmt.Sprintf("主机初始化还原不完整：%v", err))
		o.Report.Warnf("主机初始化还原不完整：%v", err)
	} else {
		o.Report.Infof("主机初始化改动已还原")
	}
	if o.Purge {
		if err := o.Deps.Host.RemoveAll(ctx, o.Paths.Kubemono()); err != nil {
			return fmt.Errorf("删除 %s 失败: %w", o.Paths.Kubemono(), err)
		}
		res.Deleted = append(res.Deleted, o.Paths.Kubemono())
		o.Report.Infof("已删除 %s", o.Paths.Kubemono())
	}
	return nil
}

// deletionList 返回第 4 步要删除的路径。
// 普通 reset 只删 kubemono 根目录下的配置入口（node.yaml、cluster.yaml、desired），
// 保留 artifacts/backup/state/hostinit.json；/usr/local/bin 与 /opt/cni/bin 也不删。
// --purge 才把整个 kubemono 根目录、运行时配置/数据、托管二进制与 CNI 插件加入清单。
func deletionList(o *Options) []string {
	list := []string{o.Paths.Kubernetes()}
	if o.Purge {
		list = append(list, o.Paths.Kubemono())
	} else {
		// 普通 reset 只清 kubemono 配置，保留 artifacts/backup/state/hostinit.json。
		list = append(list, o.Paths.NodeConfig(), o.Paths.ClusterConfig(), o.Paths.Desired())
	}
	if !o.KeepData {
		list = append(list, o.Paths.Kubelet(), o.Paths.EtcdData())
	}
	units := append([]string(nil), ComponentUnits...)
	if o.Purge {
		if o.RuntimeUnit != "" {
			units = append(units, o.RuntimeUnit)
		}
		list = append(list, purgeTargets(o)...)
	}
	for _, unit := range units {
		list = append(list, unitFilePath(o.Paths, unit))
	}
	return dedupePaths(list)
}

// purgeTargets 是 --purge 额外清理的路径：
// 二进制、CNI 插件、运行时配置/数据、审计日志和 kubemono 根目录。
func purgeTargets(o *Options) []string {
	out := []string{
		o.Paths.Kubemono(),
		o.Paths.CNIConfig(),
		o.Paths.CNIData(),
		o.Paths.Resolve("/etc/containerd"),
		o.Paths.Resolve("/var/log/kubernetes"),
	}
	if o.ContainerdDataDir != "" {
		out = append(out, o.Paths.Resolve(o.ContainerdDataDir))
	}
	out = append(out, managedBinaryPaths(o)...)
	return out
}

// managedBinaryPaths 返回 BOM 声明的宿主二进制与 CNI 插件的实际路径。
// 只删 BOM 里的名字，不整目录删除 /usr/local/bin 或 /opt/cni/bin，
// 避免误伤同机其他工具/CNI。
func managedBinaryPaths(o *Options) []string {
	b := bom.MustLoad()
	var out []string
	for _, name := range sortedComponentNames(b) {
		comp := b.Components[name]
		for _, binary := range comp.Binaries {
			base := o.Paths.LocalBin()
			if comp.Category == bom.CategoryCNI {
				base = o.Paths.CNIBin()
			}
			p := filepath.Join(base, binary)
			if _, err := os.Lstat(p); err == nil {
				out = append(out, p)
			}
		}
	}
	// kubemono 自身不在 BOM 里；如果它安装在 /usr/local/bin，--purge 也一并清掉。
	if exe, err := os.Executable(); err == nil && filepath.Clean(filepath.Dir(exe)) == filepath.Clean(o.Paths.LocalBin()) {
		out = append(out, exe)
	}
	return dedupePaths(out)
}

func sortedComponentNames(b *bom.BOM) []string {
	names := make([]string, 0, len(b.Components))
	for name := range b.Components {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func dedupePaths(paths []string) []string {
	seen := make(map[string]bool, len(paths))
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// keptDataPaths 返回 --keep-data 时保留的数据目录。
func keptDataPaths(o *Options) []string {
	if !o.KeepData {
		return nil
	}
	return []string{o.Paths.Kubelet(), o.Paths.EtcdData()}
}

// errorString 把错误转成字符串（空错误为空串）。
func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
