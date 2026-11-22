package render

import (
	"fmt"
	"strings"

	"kubemono/internal/constants"
	"kubemono/internal/profile"
	"kubemono/internal/util/fsutil"
)

// BuilderFunc 把函数适配成 Builder 接口。
type BuilderFunc struct {
	N         string
	AppliesFn func(*Context) bool
	RenderFn  func(*Context, *Spec) error
}

// Name 实现 Builder。
func (b BuilderFunc) Name() string { return b.N }

// Applies 实现 Builder。
func (b BuilderFunc) Applies(c *Context) bool {
	if b.AppliesFn == nil {
		return true
	}
	return b.AppliesFn(c)
}

// Render 实现 Builder。
func (b BuilderFunc) Render(c *Context, spec *Spec) error { return b.RenderFn(c, spec) }

// DefaultBuilders 按依赖顺序返回节点的组件 builder。
// 顺序即依赖：runtime → etcd → 控制面 → kubelet。每个 builder 自己判断是否适用，
// 因此同一份代码可以渲染 master、worker 和 etcd 节点。
func DefaultBuilders() []Builder {
	return []Builder{
		BuilderFunc{N: "runtime", AppliesFn: func(c *Context) bool {
			return c.Host != nil && c.Host.IsWorker()
		}, RenderFn: renderRuntime},
		BuilderFunc{N: "control-plane", AppliesFn: func(c *Context) bool { return c.IsMaster() }, RenderFn: renderControlPlaneBuilder},
		// etcd 以 systemd unit 运行，由 etcd-member builder 渲染：
		// stacked 形态是 master 上的 etcd.service；external 由用户管理，不渲染。
		BuilderFunc{N: "etcd-member", AppliesFn: RunsEtcd, RenderFn: renderEtcdMemberBuilder},
		// kubelet 运行在所有注册为 Node 的主机上（master 与 worker，）。
		BuilderFunc{N: "kubelet", AppliesFn: func(c *Context) bool {
			// 纯控制面 master 没有 kubelet；只有 worker（含 [master, worker]）
			// 才注册为 Node。
			return c.Host != nil && c.Host.IsWorker()
		}, RenderFn: renderKubeletBuilder},
	}
}

// renderControlPlaneBuilder 是控制面的 Builder 入口。
func renderControlPlaneBuilder(c *Context, spec *Spec) error {
	return renderControlPlane(c, spec)
}

// renderEtcdMemberBuilder 渲染 etcd.service（stacked master 或 external etcd 主机）。
func renderEtcdMemberBuilder(c *Context, spec *Spec) error {
	p, err := LoadProfile(c)
	if err != nil {
		return err
	}
	data, err := EtcdFlags(c)
	if err != nil {
		return err
	}
	data.Binary = "/usr/local/bin/etcd"
	data.ExtraArgs = ComponentExtraArgs(c, constants.ComponentEtcd)
	flags, err := p.RenderFlags(constants.ComponentEtcd, data)
	if err != nil {
		return err
	}
	unit := SystemdUnit{
		Name:             "etcd.service",
		Description:      "kubemono managed etcd (external etcd member)",
		ExecStart:        strings.Join(flags, " "),
		Restart:          "always",
		RestartSec:       "5",
		RuntimeDirectory: "etcd",
		Wants:            []string{"network-online.target"},
		After:            []string{"network-online.target"},
	}
	spec.AddFile(
		c.Paths.Resolve("/etc/systemd/system/"+unit.Name),
		[]byte(unit.Render()),
		0o644,
		constants.ComponentEtcd,
		KindUnit,
		ActionRestart,
	)
	return nil
}

// renderKubeletBuilder 渲染 kubelet 配置与 systemd unit。
func renderKubeletBuilder(c *Context, spec *Spec) error {
	data, err := KubeletFlags(c)
	if err != nil {
		return err
	}
	data.ExtraArgs = ComponentExtraArgs(c, constants.ComponentKubelet)
	return renderKubelet(c, spec, data)
}

// renderKubelet 渲染 kubelet 配置与 systemd unit。
func renderKubelet(c *Context, spec *Spec, data *profile.KubeletFlags) error {
	p, err := LoadProfile(c)
	if err != nil {
		return err
	}
	rendered, err := p.RenderConfig(constants.ComponentKubelet, data)
	if err != nil {
		return err
	}
	if len(c.Cluster.Node.Kubelet.ConfigPatch) > 0 {
		rendered, err = MergeConfigPatch(rendered, c.Cluster.Node.Kubelet.ConfigPatch)
		if err != nil {
			return fmt.Errorf("合并 kubelet.configPatch 失败: %w", err)
		}
	}
	content := fsutil.EnsureTrailingNewline([]byte(rendered))
	spec.AddFile(
		c.Paths.KubeletConfig(),
		content,
		0o644,
		constants.ComponentKubelet,
		KindConfig,
		// 修改 kubelet 配置必须重启 kubelet。
		ActionRestart,
	)
	flags, err := p.RenderFlags(constants.ComponentKubelet, data)
	if err != nil {
		return err
	}
	unit := SystemdUnit{
		Name:        constants.ComponentKubelet + ".service",
		Description: "kubemono managed kubelet",
		ExecStart:   "/usr/local/bin/kubelet " + strings.Join(flags[1:], " "),
		Wants:       []string{"network-online.target"},
		// kubelet 必须在容器运行时之后启动；自定义 CRI 时 containerd.service 不存在，
		// systemd 会忽略对不存在 unit 的排序，因此无需按运行时类型分叉。
		After:   []string{"network-online.target", "containerd.service"},
		Restart: "always",
		// kubelet 由 systemd 守护，崩溃即重启。
		RuntimeDirectory: "kubelet",
	}
	spec.AddFile(
		c.Paths.Resolve("/etc/systemd/system/"+unit.Name),
		[]byte(unit.Render()),
		0o644,
		constants.ComponentKubelet,
		KindUnit,
		ActionRestart,
	)
	return nil
}

// renderRuntime 渲染容器运行时配置；custom 由用户托管：
// containerd 的 config.toml 与 registry certs.d 目录树。
func renderRuntime(c *Context, spec *Spec) error {
	if c.Cluster.Node.Runtime.IsCustom() {
		// 自定义 CRI 由用户托管，kubemono 不写任何配置。
		return nil
	}
	rendered, err := RenderContainerdConfig(c)
	if err != nil {
		return err
	}
	spec.AddFile(
		ContainerdConfigPath(c),
		rendered,
		0o644,
		"containerd",
		KindConf,
		// certs.d / config.toml 的变更需要重启 containerd。
		ActionRestart,
	)
	for path, content := range RenderRegistryHosts(c) {
		// hosts.toml 可能含 registry 的 Basic 凭据（authorization 头），
		// 因此固定 0600 并标记 Secret：--diff 只报状态，不打印内容。
		spec.Add(File{
			Path: path, Content: content, Mode: 0o600,
			Component: "containerd", Kind: KindConf, OnChange: ActionNone, Secret: true,
		})
	}
	return nil
}

// SystemdUnit 是一个最小化的 systemd unit 渲染器。
// M5 会把它搬到 internal/systemd；现在放在渲染包里，保证 binary 模式的单元文件与
// 静态 Pod 清单使用同一份 FlagsData。
type SystemdUnit struct {
	Name             string
	Description      string
	ExecStart        string
	Wants            []string
	After            []string
	Restart          string
	RestartSec       string
	RuntimeDirectory string
	// Extra 原样追加（已是格式化好的 systemd 指令）。
	Extra []string
}

// renderExecStart 把单行 argv 渲染成 systemd 反斜杠续行的多行 ExecStart。
// 参数值不允许含空白（profile.validateFlagValue），因此按空白切分不会拆坏参数；
// 续行缩进只为人读，systemd 合并后仍按空白切分 argv。最后一个参数不带反斜杠。
func renderExecStart(execStart string) string {
	args := strings.Fields(execStart)
	if len(args) == 0 {
		return "ExecStart=\n"
	}
	var b strings.Builder
	for i, arg := range args {
		if i == 0 {
			b.WriteString("ExecStart=")
		} else {
			b.WriteString(" \\\n    ")
		}
		b.WriteString(arg)
	}
	b.WriteString("\n")
	return b.String()
}

// Render 返回 unit 文件内容。
func (u SystemdUnit) Render() string {
	var b strings.Builder
	b.WriteString("[Unit]\n")
	fmt.Fprintf(&b, "Description=%s\n", u.Description)
	for _, w := range u.Wants {
		fmt.Fprintf(&b, "Wants=%s\n", w)
	}
	for _, a := range u.After {
		fmt.Fprintf(&b, "After=%s\n", a)
	}
	b.WriteString("\n[Service]\n")
	b.WriteString("Type=simple\n")
	b.WriteString(renderExecStart(u.ExecStart))
	if u.Restart == "" {
		u.Restart = "always"
	}
	fmt.Fprintf(&b, "Restart=%s\n", u.Restart)
	if u.RestartSec == "" {
		u.RestartSec = "5"
	}
	fmt.Fprintf(&b, "RestartSec=%s\n", u.RestartSec)
	if u.RuntimeDirectory != "" {
		fmt.Fprintf(&b, "RuntimeDirectory=%s\n", u.RuntimeDirectory)
	}
	b.WriteString("LimitNOFILE=1048576\nLimitNPROC=infinity\nLimitCORE=infinity\n")
	b.WriteString("TasksMax=infinity\n")
	for _, extra := range u.Extra {
		b.WriteString(extra)
		b.WriteString("\n")
	}
	b.WriteString("\n[Install]\nWantedBy=multi-user.target\n")
	return b.String()
}
