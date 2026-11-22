package render

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"

	clusterconfig "kubemono/internal/config"
)

const (
	containerdConfigPath = "/etc/containerd/config.toml"
	containerdCertsDir   = "/etc/containerd/certs.d"
)

// RenderContainerdConfig 渲染本机节点的 containerd 配置。
// 自定义层级：字段 → configPatch；patch 覆盖结构化字段。
// 这里刻意不用 template 生成整份 config.toml：containerd 的配置项极多，
// 用结构化 TOML 生成 + patch 合并，保证不同 containerd 版本忽略未知键时不会整体失效。
func RenderContainerdConfig(c *Context) ([]byte, error) {
	if c == nil || c.Cluster == nil {
		return nil, fmt.Errorf("render: 缺少集群配置")
	}
	spec := c.Cluster
	conf := spec.Node.Runtime.Containerd

	root := conf.Root
	if root == "" {
		root = "/var/lib/containerd"
	}
	// 必须写成 containerd 1.x 的 schema（BOM 固定 containerd v1.7.24）。
	// 真实环境实测（containerd config dump）：写成 2.x 的顶层 `plugin_config` +
	// `io.containerd.cri.v1.*` 时，1.7 **整段静默忽略**——sandbox_image 退回内置的
	// pause:3.8（物料里没有这个镜像，Pod 起不来）、SystemdCgroup 退回 false
	// （与 kubelet 的 systemd cgroup 驱动不一致）、registry.config_path 失效。
	// 1.x 只认 [plugins."io.containerd.grpc.v1.cri"]。
	config := map[string]any{
		"version": 2,
		"root":    root,
		"state":   "/run/containerd",
		"plugins": map[string]any{
			"io.containerd.grpc.v1.cri": map[string]any{
				"sandbox_image": sandboxImage(c),
				// certs.d 会被热加载：镜像仓库配置变更不需要重启 containerd。
				"registry": map[string]any{"config_path": containerdCertsDir},
			},
		},
	}
	if systemdCgroup(conf) {
		// SystemdCgroup 必须先经过 cri 插件下的 runc 选项。
		cri := config["plugins"].(map[string]any)["io.containerd.grpc.v1.cri"].(map[string]any)
		cri["containerd"] = map[string]any{
			"runtimes": map[string]any{
				"runc": map[string]any{
					"runtime_type": "io.containerd.runc.v2",
					"options":      map[string]any{"SystemdCgroup": true},
				},
			},
		}
	}

	if patch := strings.TrimSpace(conf.ConfigPatch); patch != "" {
		var patched map[string]any
		if err := toml.Unmarshal([]byte(patch), &patched); err != nil {
			return nil, fmt.Errorf("解析 runtime.containerd.configPatch: %w", err)
		}
		mergeMaps(config, patched)
	}

	var buf bytes.Buffer
	enc := toml.NewEncoder(&buf)
	enc.SetIndentTables(true)
	if err := enc.Encode(config); err != nil {
		return nil, fmt.Errorf("序列化 containerd 配置: %w", err)
	}
	return buf.Bytes(), nil
}

// sandboxImage 返回该发行版的 pause 镜像。
// 必须走 Context.Image：它使用生效 BOM 里的按组件镜像覆盖，
// 否则私有仓库/离线环境里除 sandbox 外全部镜像都换了前缀，只有 pause 仍指向
// registry.k8s.io，导致所有 Pod 创建失败。
func sandboxImage(c *Context) string {
	if c == nil {
		return "registry.k8s.io/pause:3.10"
	}
	if img, err := c.Image("pause"); err == nil {
		return img
	}
	return "registry.k8s.io/pause:3.10"
}

func systemdCgroup(conf clusterconfig.ContainerdSpec) bool {
	if conf.SystemdCgroup == nil {
		return true
	}
	return *conf.SystemdCgroup
}

// RenderRegistryHosts 渲染 certs.d 下的 hosts.toml 文件（镜像仓库 mirror
// 以及各仓库的 TLS/认证配置）。返回 路径 → 内容。
//
// containerd 的 host 选择规则：按 hosts.toml 中 [host.*] 的出现顺序尝试，
// 全部失败后才用 server 兜底。因此 mirror 必须写在前面，上游只能作为
// “最后一个 host”或 server；把上游写成第一个 host 会让 mirror 永远不被使用。
func RenderRegistryHosts(c *Context) map[string][]byte {
	if c == nil || c.Cluster == nil {
		return nil
	}
	reg := c.Cluster.Node.Runtime.Containerd.Registries
	out := map[string][]byte{}

	hosts := make([]string, 0, len(reg.Mirrors))
	for host := range reg.Mirrors {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)
	for _, host := range hosts {
		var b strings.Builder
		// server 是 TOML 根键，必须在任何 [host.*] 表之前写；写在表后面会被
		// 解析成 host 表里的键，根级 server 反而丢失。上游需要认证时不用
		// server，而是把上游写成最后一个 host（认证挂在它下面）。
		if _, needAuth := reg.Configs[host]; !needAuth {
			fmt.Fprintf(&b, "server = \"https://%s\"\n", host)
		}
		for _, mirror := range reg.Mirrors[host] {
			m := strings.TrimSuffix(strings.TrimSpace(mirror), "/")
			if m == "" {
				continue
			}
			fmt.Fprintf(&b, "\n[host.%q]\n", m)
			b.WriteString("  capabilities = [\"pull\", \"resolve\"]\n")
			if auth, ok := reg.Configs[hostKeyOf(m)]; ok {
				writeRegistryAuth(&b, m, auth)
			}
		}
		if auth, ok := reg.Configs[host]; ok {
			fmt.Fprintf(&b, "\n[host.%q]\n", "https://"+host)
			b.WriteString("  capabilities = [\"pull\", \"resolve\"]\n")
			writeRegistryAuth(&b, "https://"+host, auth)
		}
		out[ContainerdCertsDir(c)+"/"+host+"/hosts.toml"] = []byte(strings.TrimLeft(b.String(), "\n") + "\n")
	}

	// 只有 configs 没有 mirrors 的仓库也要生成文件，否则 CA / 认证配置无处生效。
	configs := make([]string, 0, len(reg.Configs))
	for host := range reg.Configs {
		configs = append(configs, host)
	}
	sort.Strings(configs)
	for _, host := range configs {
		if _, ok := reg.Mirrors[host]; ok {
			continue
		}
		var b strings.Builder
		fmt.Fprintf(&b, "[host.%q]\n", "https://"+host)
		b.WriteString("  capabilities = [\"pull\", \"resolve\"]\n")
		writeRegistryAuth(&b, "https://"+host, reg.Configs[host])
		out[ContainerdCertsDir(c)+"/"+host+"/hosts.toml"] = []byte(b.String())
	}
	return out
}

// hostKeyOf 去掉 mirror URL 的 scheme，得到 configs map 的键。
func hostKeyOf(raw string) string {
	return strings.TrimPrefix(strings.TrimPrefix(raw, "https://"), "http://")
}

// writeRegistryAuth 把某个仓库端点的 TLS 与认证配置写进它自己的 host 表。
// hostURL 必须是 [host."..."] 里用的同一个 URL：containerd 要求 header 子表
// 挂在对应 host 之下（[host."<url>".header]），写错层级不会生效。
//
// 凭据以 Basic 认证头落盘（与 crictl/nerdctl 用 hosts.toml 的做法一致）；
// 因此 hosts.toml 必须是 0600 —— 见 builders.go 里 registry 文件的权限。
func writeRegistryAuth(b *strings.Builder, hostURL string, auth clusterconfig.RegistryAuthSpec) {
	if auth.CAFile != "" {
		fmt.Fprintf(b, "  ca = %q\n", auth.CAFile)
	}
	if auth.InsecureSkipVerify {
		b.WriteString("  skip_verify = true\n")
	}
	// 只写半对凭据是无意义的（containerd 会 401），校验阶段已经拒绝，
	// 这里再兜一层，避免把半对值写进文件。
	if auth.Username != "" && auth.Password != "" {
		cred := base64.StdEncoding.EncodeToString([]byte(auth.Username + ":" + auth.Password))
		fmt.Fprintf(b, "  [host.%q.header]\n", hostURL)
		fmt.Fprintf(b, "    authorization = %q\n", "Basic "+cred)
	}
}

// ContainerdConfigPath 是 kubemono 托管的 containerd 配置所在位置。
func ContainerdConfigPath(c *Context) string {
	if c != nil && c.Paths.Root != "" {
		return c.Paths.Resolve(containerdConfigPath)
	}
	return containerdConfigPath
}

// ContainerdCertsDir 是镜像仓库配置目录树。
func ContainerdCertsDir(c *Context) string {
	if c != nil && c.Paths.Root != "" {
		return c.Paths.Resolve(containerdCertsDir)
	}
	return containerdCertsDir
}
