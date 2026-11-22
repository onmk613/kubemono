// Package profile 加载唯一的组件配置基线。
// kubemono 只支持二进制部署。profile 不随 Kubernetes 小版本分目录：只保留一份
// `base/profile.yaml`，其中只有组件启动必需的核心 flag；其余参数一律使用上游
// 默认值，或由运维通过 `extraArgs` 透传覆盖。这样 Kubernetes 小版本变更时无需
// 维护新的 profile 目录、golden 文件与 removed/deprecated flag 清单。
// 渲染结果是确定的：所有 map 都按键排序后再输出。
package profile

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/template"

	clusterconfig "kubemono/internal/config"

	"sigs.k8s.io/yaml"
)

// profileFile 是每个 profile 目录的清单文件。
const profileFile = "profile.yaml"

//go:embed all:base
var profilesFS embed.FS

// 模板逻辑名。它们是 profile.yaml 中 components.<name>.templates 的键，必须全局唯一，
// 这样才能用 Template/RenderTemplate 单参数定位。控制面没有静态 Pod 清单模板；
// 只有 kube-router / local-proxy 仍使用静态 Pod。
const (
	TmplAPIServerFlags = "apiserver.flags"
	TmplKubeletConfig  = "kubelet.config"

	// 网络与负载均衡。这些组件的清单同样走 profile 模板，
	// 因此不存在任何手写 YAML 字符串拼接。
	TmplKubeRouterPodspec = "kube-router.podspec"
	// TmplKubeProxyPodspec 是 kube-proxy DaemonSet 的 Pod 定义。
	TmplKubeProxyPodspec = "kube-proxy.podspec"
	// TmplKubeProxyConfig 是 kube-proxy 的 KubeProxyConfiguration。
	TmplKubeProxyConfig = "kube-proxy.config"
	// TmplKubeRouterCNIConfig 是写进 /etc/cni/net.d 的 CNI conflist。
	TmplKubeRouterCNIConfig = "kube-router.cni.config"
	TmplLocalProxyConfig    = "local-proxy.config"
	TmplLocalProxyManifest  = "local-proxy.manifest"
)

// 模板逻辑名后缀：RenderFlags/RenderConfig 依此在组件内定位模板。
// podspecSuffix 标识"Pod 模板"：它既能嵌进静态 Pod 清单（.manifest），也能被
// 集群级控制器（kube-router 的 DaemonSet，）直接反序列化成 PodSpec，
// 因此同一份 Pod 定义不会出现两处（手写 YAML 拼接是被禁止的）。
const (
	flagsSuffix   = ".flags"
	configSuffix  = ".config"
	podspecSuffix = ".podspec"
	unitSuffix    = ".unit"
)

// Component 是一个组件的 profile 条目。
//
// 只保留"渲染真正会读"的三项：argv 前缀与模板登记。
// flags/ports/healthPath 不进入 profile：flags 只被 Load 校验、模板才是唯一
// 事实来源；ports/healthPath 无人读取，容易与代码漂移（scheduler 的健康路径、
// etcd 的 quota 都对不上）。
type Component struct {
	// Args 是 argv 的位置参数前缀，通常就是组件二进制名。
	Args []string `json:"args,omitempty"`
	// Templates 是模板逻辑名（见 Tmpl* 常量）到相对路径的映射。
	Templates map[string]string `json:"templates,omitempty"`
}

// Profile 是一个 Kubernetes 小版本的组件配置 profile。
// Load 返回的 *Profile 是共享的只读对象，调用方不得修改其中的 map/slice。
type Profile struct {
	// Version 是 profile 版本，等于目录名，例如 v1.37。
	Version string `json:"version"`
	// Components 是组件名到组件配置的映射。
	Components map[string]Component `json:"components"`
}

// FlagsData 是 flags/config/静态 Pod 模板的渲染数据：kubemono 在渲染某个节点的
// desired 状态时计算出的全部组件参数。字段名与模板中的占位符一一对应。
// 调用方（internal/render、internal/controlplane）负责把 cluster.yaml、节点身份和 BOM
// 组合成这里的值。
// CommonFlags 是每个组件模板数据共享的公共字段。
// 组件模板各自持有专属字段（见 APIServerFlags 等），共享的只有这几项。
type CommonFlags struct {
	Binary      string            // 组件二进制绝对路径 / absolute path of the component binary
	Image       string            // 静态 Pod 使用的镜像（含 tag） / image used by the static Pod (tag included)
	Hostname    string            // 节点名 / node name
	ConfigHash  string            // 渲染内容哈希，写入 kubemono.io/config-hash / hash of the rendered content, written to kubemono.io/config-hash
	RestartedAt string            // 强制重启时间戳，写入 kubemono.io/restarted-at / forced restart timestamp, written to kubemono.io/restarted-at
	ExtraArgs   map[string]string // 操作员的 extraArgs，覆盖同名默认 flag / operator extraArgs, overriding default flags of the same name
}

// 以下组件结构按模板的真实字段清单划分，避免共享数据袋导致 ExtraArgs
// 预填错组件、模板层无法隔离。
// 跨模板重复的字段（如 ClientCAFile 同时被 apiserver/controller-manager/kubelet
// 配置引用）是刻意的：各组件只依赖自己的结构，清单演进互不影响。

// APIServerFlags 是 kube-apiserver flags 模板的数据。
type APIServerFlags struct {
	CommonFlags
	AdvertiseAddress             string
	APIServerPort                int
	ServiceCIDR                  string
	ClusterDomain                string
	APIServerCertFile            string
	APIServerKeyFile             string
	ClientCAFile                 string
	KubeletClientCertFile        string
	KubeletClientKeyFile         string
	FrontProxyCACertFile         string
	FrontProxyClientCertFile     string
	FrontProxyClientKeyFile      string
	ServiceAccountPubFile        string
	ServiceAccountPrivateKeyFile string
	EtcdServers                  []string
	EtcdCAFile                   string
	EtcdCertFile                 string
	EtcdKeyFile                  string
	AuditEnabled                 bool
	AuditPolicyFile              string
	AuditLogFile                 string
	AuditMaxAge                  int
	AuditMaxBackup               int
	AuditMaxSize                 int
	AuditPolicy                  string
	EncryptionEnabled            bool
	EncryptionConfigFile         string
}

// ControllerManagerFlags 是 kube-controller-manager flags 模板的数据。
type ControllerManagerFlags struct {
	CommonFlags
	ControllerManagerKubeconfig  string
	ClusterCIDR                  string
	NodeCIDRMaskSize             int32
	ClusterSigningCertFile       string
	ClusterSigningKeyFile        string
	RootCAFile                   string
	ClientCAFile                 string
	FrontProxyCACertFile         string
	ServiceAccountPrivateKeyFile string
}

// SchedulerFlags 是 kube-scheduler flags 模板的数据。
type SchedulerFlags struct {
	CommonFlags
	SchedulerKubeconfig string
}

// EtcdFlags 是本机 etcd 成员（stacked/external 成员）flags 模板的数据。
// apiserver 访问 etcd 的客户端证书不在这里（见 APIServerFlags.EtcdCertFile）。
type EtcdFlags struct {
	CommonFlags
	EtcdName                     string
	EtcdDataDir                  string
	EtcdServerTrustedCAFile      string
	EtcdServerCertFile           string
	EtcdServerKeyFile            string
	EtcdPeerTrustedCAFile        string
	EtcdPeerCertFile             string
	EtcdPeerKeyFile              string
	EtcdListenClientURLs         []string
	EtcdAdvertiseClientURLs      []string
	EtcdListenPeerURLs           []string
	EtcdInitialAdvertisePeerURLs []string
	EtcdInitialCluster           []string
	EtcdInitialClusterState      string
	EtcdMetricsURLs              string
	EtcdSnapshotCount            int
	EtcdQuotaBackendBytes        int64
	EtcdAutoCompactionRetention  string
}

// KubeletFlags 是 kubelet flags 与 kubelet-config.yaml 模板共用的数据
// （两个模板引用同一份运行参数）。
type KubeletFlags struct {
	CommonFlags
	KubeletConfigFile        string
	KubeletKubeconfigFile    string
	BootstrapKubeconfigFile  string
	KubeletCertDir           string
	KubeletRootDir           string
	ContainerRuntimeEndpoint string
	NodeIP                   string
	KubeletClusterDNS        []string
	KubeletResolvConf        string
	// KubeletCgroupDriver 是 kubelet 使用的 cgroup 驱动：
	// 托管 containerd 固定 systemd；自定义 CRI 由 node.runtime.custom.cgroupDriver 决定。
	KubeletCgroupDriver string
	// RegisterWithTaints 是 kubelet 注册节点时自带的污点：
	// 控制面节点默认带 node-role.kubernetes.io/control-plane:NoSchedule，
	// 再加上 cluster.hosts[].taints。
	RegisterWithTaints []string
	ClientCAFile       string
	ClusterDomain      string
}

// KubeProxyFlags 是 kube-proxy 的 config/flags/podspec 三个模板共用的数据。
type KubeProxyFlags struct {
	CommonFlags
	// KubeProxyMode 是 KubeProxyConfiguration.mode（ipvs/iptables/nftables）。
	KubeProxyMode string
	// KubeProxyScheduler 是 IPVS 调度器，仅 mode=ipvs 时写入配置。
	KubeProxyScheduler  string
	KubeProxyConfigFile string
	// KubeProxyKubeconfigFile 是容器内 ServiceAccount kubeconfig 的路径。
	KubeProxyKubeconfigFile string
	KubeProxyConfigMapName  string
	ClusterCIDR             string
	KubeProxyHealthzPort    uint16
	KubeProxyMetricsPort    uint16
}

// KubeRouterFlags 是 kube-router 的 flags/podspec 两个模板共用的数据。
type KubeRouterFlags struct {
	CommonFlags
	KubeRouterRunRouter      bool
	KubeRouterRunFirewall    bool
	KubeRouterKubeconfigFile string
	// KubeRouterConfigMapName 是挂载进 Pod 的 ConfigMap（含 SA kubeconfig，）。
	KubeRouterConfigMapName string
	// KubeRouterHealthPort 是 kube-router 的健康检查端口（v2.4.1 的 --health-port，0=关闭）。
	KubeRouterHealthPort uint16
	// KubeRouterMetricsPort 是 kube-router 的指标端口（v2.4.1 的 --metrics-port，0=关闭）。
	KubeRouterMetricsPort   uint16
	KubeRouterClusterASN    int64
	KubeRouterEnableOverlay bool
	KubeRouterOverlayType   string
}

// flagsCommon 让每个组件结构共享 RenderFlags 需要的公共字段访问：
// ExtraArgs（覆盖默认 flag）与 Binary（argv[0] 默认值）。
type flagsCommon interface {
	FlagsCommon() *CommonFlags
}

// FlagsCommon 实现同名接口：嵌入 CommonFlags 的组件结构自动满足。
func (c *CommonFlags) FlagsCommon() *CommonFlags { return c }

// LocalProxyBackend 是本地代理的一个 apiserver 后端。
type LocalProxyBackend struct {
	// Name 是 haproxy 的 server 名，取自 master 名，保证渲染确定。
	Name string
	// Address 是 apiserver 的 IP 或主机名。
	Address string
	// Port 是 apiserver 端口，默认 6443。
	Port int
}

// KubeRouterCNIData 是 CNI conflist 模板的渲染数据。
// 每台节点的 podCIDR 不同，因此 conflist 必须按节点渲染。
type KubeRouterCNIData struct {
	// Name 是 CNI 网络名，固定为 kubemono。
	Name string
	// NodeCIDR 是 kube-controller-manager 分配给本节点的 Node.spec.podCIDR；
	// 未分配时退回 podCIDR + nodeCIDRMaskSize 推导出的第一个子网。
	NodeCIDR string
	// MTU 是建议的 MTU；0 表示由内核决定，模板里省略该字段。
	MTU int
	// ServiceCIDR 让 Pod 到 Service 的流量不被 bridge 插件劫持。
	ServiceCIDR string
}

// LocalProxyData 是本地代理模板的渲染数据。
// 后端列表是唯一的变量，因此后端增删只会让"这一个文件"变化。
type LocalProxyData struct {
	Binary      string
	Image       string
	Hostname    string
	ConfigHash  string
	RestartedAt string
	ConfigFile  string
	ProxyPort   int
	StatsPort   int
	Backends    []LocalProxyBackend
}

// loadOnce 缓存唯一的 base profile（成功与失败都缓存）。
// 嵌入内容不会在进程内变化，因此不需要按 name 建 map，也不需要手写锁。
var loadOnce = sync.OnceValues(func() (*Profile, error) {
	const name = "base"
	data, err := fs.ReadFile(profilesFS, name+"/"+profileFile)
	if err != nil {
		return nil, fmt.Errorf("profile %q 不存在: %w", name, err)
	}
	p := &Profile{}
	if err := yaml.UnmarshalStrict(data, p); err != nil {
		return nil, fmt.Errorf("profile %s: 解析 %s 失败: %w", name, profileFile, err)
	}
	p.Version = name
	if err := p.validate(); err != nil {
		return nil, fmt.Errorf("profile %s: %w", name, err)
	}
	return p, nil
})

// Load 读取并校验唯一的 base profile。
// 解析成功的结果会被缓存；返回的对象是只读的。
func Load() (*Profile, error) { return loadOnce() }

// validate 检查 profile 的自洽性：模板文件存在、逻辑模板名全局唯一且后缀合法。
func (p *Profile) validate() error {
	if len(p.Components) == 0 {
		return fmt.Errorf("components 不能为空")
	}
	seenTemplate := map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(p.Components)) {
		c := p.Components[name]
		for _, logical := range slices.Sorted(maps.Keys(c.Templates)) {
			rel := c.Templates[logical]
			if logical == "" || rel == "" {
				return fmt.Errorf("组件 %s 存在空的模板登记项", name)
			}
			if prev, ok := seenTemplate[logical]; ok {
				return fmt.Errorf("模板逻辑名 %q 被组件 %s 和 %s 重复登记", logical, prev, name)
			}
			seenTemplate[logical] = name
			if _, err := fs.Stat(profilesFS, p.Version+"/"+rel); err != nil {
				return fmt.Errorf("组件 %s 的模板 %s 指向不存在的文件 %s", name, logical, rel)
			}
			if !strings.HasSuffix(logical, flagsSuffix) && !strings.HasSuffix(logical, configSuffix) &&
				!strings.HasSuffix(logical, podspecSuffix) && !strings.HasSuffix(logical, unitSuffix) &&
				!strings.HasSuffix(logical, ".manifest") {
				return fmt.Errorf("组件 %s 的模板逻辑名 %q 必须以 %s、%s、%s、%s 或 .manifest 结尾",
					name, logical, flagsSuffix, configSuffix, podspecSuffix, unitSuffix)
			}
		}
	}
	return nil
}

// sortedComponentNames 返回排序后的组件名。
// ComponentNames 返回全部组件名，已排序。
func (p *Profile) ComponentNames() []string { return slices.Sorted(maps.Keys(p.Components)) }

// Component 返回指定组件的配置。
func (p *Profile) Component(name string) (Component, error) {
	c, ok := p.Components[name]
	if !ok {
		return Component{}, fmt.Errorf("profile %s: 未知组件 %q，已知组件：%s", p.Version, name, strings.Join(p.ComponentNames(), ", "))
	}
	return c, nil
}

// Template 返回模板逻辑名对应的模板源文件内容。
func (p *Profile) Template(name string) (string, error) {
	rel, err := p.templatePath(name)
	if err != nil {
		return "", err
	}
	data, err := fs.ReadFile(profilesFS, p.Version+"/"+rel)
	if err != nil {
		return "", fmt.Errorf("profile %s: 读取模板 %s (%s) 失败: %w", p.Version, name, rel, err)
	}
	return string(data), nil
}

// templatePath 把逻辑模板名解析为相对路径。
func (p *Profile) templatePath(name string) (string, error) {
	for _, component := range p.ComponentNames() {
		if rel, ok := p.Components[component].Templates[name]; ok {
			return rel, nil
		}
	}
	return "", fmt.Errorf("profile %s: 未知模板 %q，已知模板：%s", p.Version, name, strings.Join(p.TemplateNames(), ", "))
}

// TemplateNames 返回全部模板逻辑名，已排序。
func (p *Profile) TemplateNames() []string {
	var out []string
	for _, component := range p.ComponentNames() {
		for name := range p.Components[component].Templates {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// RenderTemplate 用 data 渲染逻辑模板 name。模板里的 map 迭代一律经过排序辅助函数，
// 因此同一份 data 的输出是确定的。
func (p *Profile) RenderTemplate(name string, data any) (string, error) {
	return p.renderTemplate(name, data)
}

// renderTemplate 是 RenderTemplate 和 include 的共用实现。
func (p *Profile) renderTemplate(name string, data any) (string, error) {
	src, err := p.Template(name)
	if err != nil {
		return "", err
	}
	t, err := template.New(name).Funcs(p.funcMap(data)).Option("missingkey=error").Parse(src)
	if err != nil {
		return "", fmt.Errorf("profile %s: 解析模板 %s 失败: %w", p.Version, name, err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("profile %s: 渲染模板 %s 失败: %w", p.Version, name, err)
	}
	return buf.String(), nil
}

// funcMap 提供模板辅助函数。include 捕获当前 data，以便静态 Pod 清单嵌入同一份 flags。
// taintKey/taintValue/taintEffect 把 cluster.yaml 里的 "key[=value]:Effect" 形式拆成
// KubeletConfiguration 的结构。解析统一走 clusterconfig（见 ParseTaint/LenientTaint）。
func taintKey(taint string) string {
	key, _, _ := clusterconfig.LenientTaint(taint)
	return key
}

func taintValue(taint string) string {
	_, value, _ := clusterconfig.LenientTaint(taint)
	return value
}

func taintEffect(taint string) string {
	_, _, effect := clusterconfig.LenientTaint(taint)
	return effect
}

func (p *Profile) funcMap(data any) template.FuncMap {
	return template.FuncMap{
		"include": func(name string) (string, error) { return p.renderTemplate(name, data) },
		"flags": func(component string) ([]string, error) {
			return p.RenderFlags(component, data)
		},
		"argvSeq":     argvSeq,
		"taintKey":    taintKey,
		"taintValue":  taintValue,
		"taintEffect": taintEffect,
		"join":        func(sep string, elems []string) string { return strings.Join(elems, sep) },
		"quote":       strconv.Quote,
		"indent":      indent,
		// appendArgs 的参数顺序与 Go 内置 append 一致：appendArgs(base, extra)。
		// 模板里写成 `{{ flags "x" | appendArgs .Extra }}`：管道值作为最后一个参数，
		// 因此实际调用是 appendArgs(.Extra, flags)，由本函数负责换回正确顺序。
		"appendArgs": appendArgs,
	}
}

// appendArgs 把 flags 与运行期参数拼成一个 argv。
// 参数顺序按模板管道的需要设计：模板写 `{{ flags "x" | appendArgs .Extra }}` 时，
// 管道值（.Extra）会成为最后一个参数，因此这里的第一个参数是运行期参数、
// 第二个是 flags，返回值是 flags + 运行期参数（argv[0] 必须是二进制）。
// 不修改入参：profile 返回的 FlagsData 可能被多个模板共享。
func appendArgs(runtimeArgs, flags []string) []string {
	out := make([]string, 0, len(flags)+len(runtimeArgs))
	out = append(out, flags...)
	out = append(out, runtimeArgs...)
	return out
}

// indent 把多行文本整体右移 n 个空格，空行保持为空。
// 用于把 .podspec 模板内联进静态 Pod 清单（YAML 的缩进是语义的一部分）。
func indent(n int, s string) string {
	pad := strings.Repeat(" ", n)
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, line := range lines {
		if line == "" {
			continue
		}
		lines[i] = pad + line
	}
	return strings.Join(lines, "\n")
}

// RenderFlags 渲染组件的 flags 模板，返回完整 argv（argv[0] 是二进制）。
// data 是组件专属的模板数据（APIServerFlags/EtcdFlags/...）：只有嵌入
// CommonFlags 的结构才被接受，ExtraArgs 与 Binary 默认值从公共部分读取。
// 处理顺序：
// 1. 渲染组件登记的 .flags 模板，按行切成 argv；
// 2. 丢弃与 extraArgs 同名的默认 flag（extraArgs 覆盖，避免 StringSlice 之类的 flag
// 被追加两次）；
// 3. 拒绝不支持的 flag（模板默认值和 extraArgs 都检查）；
// 4. 追加排序后的 extraArgs。
func (p *Profile) RenderFlags(component string, data any) ([]string, error) {
	c, err := p.Component(component)
	if err != nil {
		return nil, err
	}
	name, err := p.flagsTemplateName(component)
	if err != nil {
		return nil, err
	}
	common := flagsCommonOf(data)
	if common == nil {
		return nil, fmt.Errorf("profile %s: 模板 %s 需要 embedding CommonFlags 的组件数据（%T 不满足）", p.Version, name, data)
	}
	extraArgs := common.ExtraArgs
	if common.Binary == "" {
		common.Binary = binaryOf(c, component)
	}
	rendered, err := p.RenderTemplate(name, data)
	if err != nil {
		return nil, err
	}
	lines := splitArgs(rendered)
	if len(lines) == 0 {
		return nil, fmt.Errorf("profile %s: 模板 %s 没有渲染出 argv", p.Version, name)
	}
	out := make([]string, 0, len(lines)+len(extraArgs))
	out = append(out, lines[0])
	for _, arg := range lines[1:] {
		flag, ok := flagNameOf(arg)
		if !ok {
			return nil, fmt.Errorf("profile %s: 模板 %s 渲染出非法参数 %q", p.Version, name, arg)
		}
		if err := validateFlagName(flag); err != nil {
			return nil, fmt.Errorf("profile %s 组件 %s: %w", p.Version, component, err)
		}
		if _, overridden := extraArgs[flag]; overridden {
			continue
		}
		out = append(out, arg)
	}
	extra, err := extraFlags(extraArgs)
	if err != nil {
		return nil, fmt.Errorf("profile %s 组件 %s: %w", p.Version, component, err)
	}
	out = append(out, extra...)
	return out, nil
}

// flagsCommonOf 取组件数据的公共字段；只接受指针形态（值拷贝上改 Binary
// 会丢失，模板数据一律按指针传递）。
func flagsCommonOf(data any) *CommonFlags {
	if c, ok := data.(flagsCommon); ok {
		return c.FlagsCommon()
	}
	return nil
}

// RenderConfig 渲染组件的配置文件模板（kubelet）。
// data 是组件专属模板数据（KubeletFlags），与 RenderFlags 同一套约束。
func (p *Profile) RenderConfig(component string, data any) (string, error) {
	name, err := p.configTemplateName(component)
	if err != nil {
		return "", err
	}
	return p.RenderTemplate(name, data)
}

// flagsTemplateName 返回组件登记的 .flags 模板逻辑名。
func (p *Profile) flagsTemplateName(component string) (string, error) {
	return p.singleTemplate(component, flagsSuffix)
}

// configTemplateName 返回组件登记的 .config 模板逻辑名。
func (p *Profile) configTemplateName(component string) (string, error) {
	return p.singleTemplate(component, configSuffix)
}

// singleTemplate 在组件内按后缀找唯一的模板逻辑名。
func (p *Profile) singleTemplate(component, suffix string) (string, error) {
	c, err := p.Component(component)
	if err != nil {
		return "", err
	}
	var found []string
	for name := range c.Templates {
		if strings.HasSuffix(name, suffix) {
			found = append(found, name)
		}
	}
	sort.Strings(found)
	switch len(found) {
	case 0:
		return "", fmt.Errorf("profile %s: 组件 %s 没有 %s 模板", p.Version, component, suffix)
	case 1:
		return found[0], nil
	default:
		return "", fmt.Errorf("profile %s: 组件 %s 登记了多个 %s 模板：%s", p.Version, component, suffix, strings.Join(found, ", "))
	}
}

// binaryOf 返回组件 argv 的二进制名：优先用 Args[0]，否则用组件名。
func binaryOf(c Component, component string) string {
	if len(c.Args) > 0 && c.Args[0] != "" {
		return c.Args[0]
	}
	return component
}

// flagNameOf 解析 argv 元素里的 flag 名。
func flagNameOf(arg string) (string, bool) {
	if !strings.HasPrefix(arg, "--") {
		return "", false
	}
	name := strings.TrimPrefix(arg, "--")
	if i := strings.IndexByte(name, '='); i >= 0 {
		name = name[:i]
	}
	return name, true
}

// validateFlagName 校验 flag 名。规则来自 internal/config，保证
// `config validate` 通过的名字在渲染阶段也一定通过，不会晚失败。
func validateFlagName(name string) error {
	return clusterconfig.ValidateExtraArgName(name)
}

// validateFlagValue 校验 flag 值：不能含空白（会破坏 argv 切分）。
func validateFlagValue(flag, value string) error {
	if strings.ContainsAny(value, " \t\n\r") {
		return fmt.Errorf("flag --%s 的值 %q 含空白；argv/ExecStart 按空白切分，请改用不含空白的值", flag, value)
	}
	return nil
}

// argvSeq 把 argv 转成 YAML 流式序列（JSON 是 YAML 的子集），供静态 Pod 清单的
// command 使用。入参可以是"每行一个参数"的文本，也可以是已经切分好的 []string。
func argvSeq(v any) (string, error) {
	switch t := v.(type) {
	case string:
		return jsonArray(splitArgs(t))
	case []string:
		return jsonArray(t)
	default:
		return "", fmt.Errorf("argvSeq: 不支持的类型 %T", v)
	}
}

// jsonArray 用 JSON 编码字符串数组，关闭 HTML 转义以保证输出稳定可读。
func jsonArray(items []string) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if items == nil {
		items = []string{}
	}
	if err := enc.Encode(items); err != nil {
		return "", fmt.Errorf("编码 YAML 序列失败: %w", err)
	}
	return strings.TrimRight(buf.String(), "\n"), nil
}

// splitArgs 把模板输出切成 argv：去掉空行和行首尾空白。
func splitArgs(text string) []string {
	var out []string
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		out = append(out, line)
	}
	return out
}

// extraFlags 把 extraArgs 渲染成排序后的 --k=v 列表。
func extraFlags(extra map[string]string) ([]string, error) {
	out := make([]string, 0, len(extra))
	for _, flag := range slices.Sorted(maps.Keys(extra)) {
		if err := validateFlagName(flag); err != nil {
			return nil, err
		}
		value := extra[flag]
		if value == "" {
			continue
		}
		if err := validateFlagValue(flag, value); err != nil {
			return nil, err
		}
		out = append(out, "--"+flag+"="+value)
	}
	return out, nil
}
