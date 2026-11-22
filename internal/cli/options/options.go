package options

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"kubemono/internal/bom"
	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	"kubemono/internal/lock"
	"kubemono/internal/util/output"
)

// GlobalOptions 保存 kubemono 各命令共享的选项值
type GlobalOptions struct {
	// ConfigPath 是集群配置文件（-f/--config）。
	ConfigPath string
	// Node 强制指定本机的节点标识
	Node string
	// AssumeYes 跳过破坏性命令的交互确认（-y/--assume-yes）。
	AssumeYes bool
	// AllowAddressMismatch 允许 --node 指定的节点地址不在本机网卡上（NAT 场景）。
	AllowAddressMismatch bool
	// Verbose 启用调试输出。
	Verbose bool
	// DryRun 只报告操作，不修改主机
	DryRun bool
	// Output 选择机器可读格式：table|json|yaml
	Output string
	// DataDir 是 kubemono 自身数据目录（默认 /usr/local/kubemono）。
	DataDir string
	// TestRoot 仅测试使用：把所有约定路径重定位到临时根，避免写真实主机。
	// 不是 CLI 参数；生产代码不应设置。
	TestRoot string
	// Wait 限制 `install` 等阻塞操作的最长等待时间。
	Wait time.Duration
	// KubeconfigPath 是集群级命令使用的凭据。
	// 为空时在 master 上使用 /etc/kubernetes/admin.conf。
	KubeconfigPath string
	// Reporter 输出进度消息。
	Reporter *output.Console
}

// Default 返回生产 CLI 的默认选项。
func Default() *GlobalOptions {
	return &GlobalOptions{
		Output:   "table",
		Wait:     constants.DefaultInstallWait,
		Reporter: output.NewConsole(),
	}
}

// Paths 返回本次调用使用的路径解析器。
func (o *GlobalOptions) Paths() constants.Paths {
	p := constants.NewPaths(o.TestRoot)
	if o.DataDir != "" {
		p = p.WithKubemonoDir(o.DataDir)
	}
	return p
}

// ReporterOrDiscard 返回一个非 nil 的 reporter。
func (o *GlobalOptions) ReporterOrDiscard() output.Reporter {
	if o.Reporter == nil {
		return output.Discard()
	}
	return o.Reporter
}

// ClusterConfigPath 返回本次调用应读取的 cluster.yaml 路径与是否存在。
// 显式 -f 一律视为“存在”（读不到时由 LoadCluster 报错）；默认路径按本机缓存探测。
func (o *GlobalOptions) ClusterConfigPath() (string, bool) {
	if o.ConfigPath != "" {
		return o.ConfigPath, true
	}
	p := o.Paths().ClusterConfig()
	if _, err := os.Stat(p); err != nil {
		return p, false
	}
	return p, true
}

// LoadCluster 读取并校验集群配置。
// 查找顺序：显式 -f → 本机缓存 /usr/local/kubemono/cluster.yaml。
// 前者用于首次安装和变更提交，后者用于已安装节点上的日常命令。
func (o *GlobalOptions) LoadCluster() (*clusterconfig.Cluster, error) {
	path, _ := o.ClusterConfigPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("cluster configuration %s not found: pass -f cluster.yaml", path)
		}
		return nil, err
	}
	cluster, err := clusterconfig.LoadCluster(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	// 组件版本与镜像覆盖在加载阶段就拒绝未知组件/kubemono 自身；任意版本都允许，
	// 非默认版本只告警，避免后面执行到一半才失败。
	if _, err := o.EffectiveBOM(cluster); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for _, warning := range cluster.Warnings {
		o.ReporterOrDiscard().Warnf("%s", warning)
	}
	return cluster, nil
}

// EffectiveBOM 返回应用了集群组件版本固定值的 BOM。
// render/install/phase 必须使用该 BOM；内嵌 BOM 本身只描述发行版默认值，
// 而不是集群期望的版本。
func (o *GlobalOptions) EffectiveBOM(cluster *clusterconfig.Cluster) (*bom.BOM, error) {
	if cluster == nil {
		return bom.MustLoad(), nil
	}
	return bom.ApplyOverrides(bom.MustLoad(), cluster.VersionOverrides(), cluster.ImageOverrides(), func(w string) {
		o.ReporterOrDiscard().Warnf("%s", w)
	})
}

// LockForWrite 获取数据目录的跨进程写锁；DryRun 不获取。
// 调用方必须在整个写操作期间持有它，并 defer Close。
func (o *GlobalOptions) LockForWrite() (*lock.Lock, error) {
	if o.DryRun {
		return nil, nil
	}
	path := filepath.Join(o.Paths().State(), "kubemono.lock")
	lk, err := lock.Acquire(path)
	if err != nil {
		return nil, fmt.Errorf("%w；请等另一个 init/join/node repair/cert renew/reset 结束后重试", err)
	}
	return lk, nil
}
