// Package network 渲染 CNI 与 Service 代理的本机文件和集群对象。
// CNI 由 network.plugin 决定（kube-router/none）；Service 代理默认由
// kube-proxy DaemonSet 承担，kube-router 不自动替换 kube-proxy。
package network

import (
	"fmt"
	"sort"

	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	"kubemono/internal/render"
	"kubemono/internal/util/fsutil"
)

// Health 描述网络层某个组件的健康端点。
type Health struct {
	// Component 是所属组件名。
	Component string
	// Scheme 是 http 或 https。
	Scheme string
	// Address 是 host:port。
	Address string
	// Path 是端点路径。
	Path string
}

// kubelet 挂载的 ServiceAccount 凭据在 Pod 内的路径。
const (
	// ServiceAccountTokenPath 是 Pod 的 ServiceAccount 投射令牌路径。
	ServiceAccountTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	// ServiceAccountCAPath 是与令牌一同挂载的集群 CA 包。
	ServiceAccountCAPath = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	// DaemonSetKubeconfigKey 是存放 kubeconfig 的 ConfigMap 键。
	DaemonSetKubeconfigKey = "kubeconfig.conf"
)

// ServiceAccountKubeconfig 返回 hostNetwork Pod 用 SA token 访问本机 apiserver 的 kubeconfig。
// 使用 127.0.0.1 避免依赖 ClusterIP；文件不含密钥，可放入 ConfigMap。
func ServiceAccountKubeconfig(user, server string) []byte {
	if server == "" {
		server = fmt.Sprintf("https://127.0.0.1:%d", constants.APIServerPort)
	}
	return []byte(fmt.Sprintf(`# %s 以 hostNetwork 运行：服务端固定为 %s，身份是 Pod 自己的 ServiceAccount token。
apiVersion: v1
kind: Config
clusters:
  - name: kubemono-local
    cluster:
      server: %s
      certificate-authority: %s
users:
  - name: %s
    user:
      tokenFile: %s
contexts:
  - name: kubemono-local
    context:
      cluster: kubemono-local
      user: %s
current-context: kubemono-local
`, user, server, server, ServiceAccountCAPath, user, ServiceAccountTokenPath, user))
}

// String 渲染探针 URL。
func (h Health) String() string { return fmt.Sprintf("%s://%s%s", h.Scheme, h.Address, h.Path) }

// Plan 是网络层在单个节点上的完整期望状态。
type Plan struct {
	// Files 是本节点的本机文件，按路径排序。
	Files []render.File
	// Objects 是集群级对象，按创建顺序排列。
	Objects []render.Object
	// Healths 是网络组件的健康端点。
	Healths []Health
}

// Spec 把计划转换为 render.Spec（仅文件）。
func (p *Plan) Spec() *render.Spec {
	spec := &render.Spec{}
	for _, f := range p.Files {
		spec.Add(f)
	}
	spec.Sort()
	return spec
}

// Build 返回本节点的网络期望状态（本机文件 + 集群对象）。
// nodeCIDR 为空时按 podCIDR 和 nodeCIDRMaskSize 推导。
// 网络对象的统一归属标签：prune 按标签列对象，不依赖本地状态文件。
const (
	// ManagedByLabel 标记对象由 kubemono 管理。
	ManagedByLabel = "app.kubernetes.io/managed-by"
	// ManagedByValue 是 ManagedByLabel 的固定值。
	ManagedByValue = "kubemono"
	// NetworkComponentLabel 标记对象属于网络层。
	NetworkComponentLabel = "kubemono.io/component"
	// NetworkComponentValue 是 NetworkComponentLabel 的固定值。
	NetworkComponentValue = "network"
)

// ManagedLabels 返回带归属标签的副本。
func ManagedLabels(base map[string]string) map[string]string {
	out := make(map[string]string, len(base)+2)
	for k, v := range base {
		out[k] = v
	}
	out[ManagedByLabel] = ManagedByValue
	out[NetworkComponentLabel] = NetworkComponentValue
	return out
}

// ManagedSelector 是列出网络层全部对象的 label selector。
func ManagedSelector() string {
	return ManagedByLabel + "=" + ManagedByValue + "," + NetworkComponentLabel + "=" + NetworkComponentValue
}

func Build(c *render.Context, nodeCIDR string) (*Plan, error) {
	if c == nil || c.Cluster == nil {
		return nil, fmt.Errorf("network: 缺少集群配置")
	}
	cfg := c.Cluster.Network

	// local 汇总本机文件；同路径内容冲突时报错。
	local := &localSpec{}
	var objects []render.Object
	var healths []Health
	// 纯控制面 master 不跑 CNI，也不写任何本机网络文件；只把集群级对象
	// （kube-router/kube-proxy 的 DS/RBAC/ConfigMap）交给 apiserver。
	worker := c.Host != nil && c.Host.IsWorker()

	// ---- CNI 插件 ----
	switch cfg.Plugin {
	case clusterconfig.PluginKubeRouter:
		part, err := kubeRouterCNIPlan(c, nodeCIDR)
		if err != nil {
			return nil, err
		}
		if worker {
			for _, f := range part.Files {
				if err := local.add(f); err != nil {
					return nil, err
				}
			}
			// kube-router 负责 CNI，必须探活（附录 A：20244）。
			healths = append(healths, part.Healths...)
		}
		objects = append(objects, part.Objects...)
	case clusterconfig.PluginNone:
		// 不写 CNI 配置；插件由 runtime phase 安装。
	default:
		return nil, fmt.Errorf("未知的 network.plugin %q：期望 %s 或 %s", cfg.Plugin, clusterconfig.PluginKubeRouter, clusterconfig.PluginNone)
	}

	// ---- Service 代理 ----
	// 与 CNI 插件解耦：plugin=none 时仍默认部署 kube-proxy；使用自带
	// kube-proxy replacement 的 CNI 时用 network.kubeProxy.enabled=false 关闭。
	proxyPlan, err := kubeProxyPlan(c)
	if err != nil {
		return nil, err
	}
	objects = append(objects, proxyPlan.Objects...)
	// 纯控制面 master 不运行 kubelet，也就不会跑 kube-proxy DaemonSet；
	// 把它的健康端点加进来会让 network.Check 永远失败、每次 repair 都重跑。
	if worker {
		healths = append(healths, proxyPlan.Healths...)
	}

	files, err := local.files()
	if err != nil {
		return nil, err
	}
	objects, err = dedupObjects(objects)
	if err != nil {
		return nil, err
	}
	return &Plan{
		Files:   files,
		Objects: objects,
		Healths: dedupHealths(healths),
	}, nil
}

// localSpec 汇总本机文件并去重。
type localSpec struct {
	list []render.File
}

// add 合并一个文件，拒绝冲突的重复项。
func (l *localSpec) add(f render.File) error {
	for _, existing := range l.list {
		if existing.Path != f.Path {
			continue
		}
		if existing.SHA256() != f.SHA256() {
			return fmt.Errorf("network: %s 被渲染了两次且内容不一致（组件 %s 与 %s）", f.Path, existing.Component, f.Component)
		}
		return nil
	}
	l.list = append(l.list, f)
	return nil
}

// files 返回按路径排序的已汇总文件。
func (l *localSpec) files() ([]render.File, error) {
	out := append([]render.File(nil), l.list...)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// dedupObjects 去重并校验对象内容一致。
func dedupObjects(in []render.Object) ([]render.Object, error) {
	seen := map[string]string{}
	out := make([]render.Object, 0, len(in))
	for _, o := range in {
		data, err := o.Data()
		if err != nil {
			return nil, err
		}
		sum := fsutil.SHA256Bytes(data)
		if prev, ok := seen[o.Ref()]; ok {
			if prev != sum {
				return nil, fmt.Errorf("network: 集群对象 %s 被渲染了两次且内容不一致", o.Ref())
			}
			continue
		}
		seen[o.Ref()] = sum
		out = append(out, o)
	}
	return out, nil
}

// dedupHealths 移除重复的健康端点。
func dedupHealths(in []Health) []Health {
	seen := map[string]bool{}
	out := make([]Health, 0, len(in))
	for _, h := range in {
		key := h.String()
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, h)
	}
	return out
}
