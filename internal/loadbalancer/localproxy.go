package loadbalancer

import (
	"fmt"
	"strconv"
	"strings"

	"kubemono/internal/constants"
	"kubemono/internal/profile"
	"kubemono/internal/render"
	"kubemono/internal/util/fsutil"
)

// Backend 是本地代理的一个 apiserver 端点。
type Backend struct {
	// Name 是 haproxy 的 server 名；它取自 master 主机名，
	// 以保证渲染结果确定、diff 可读。
	Name string `json:"name"`
	// Address 是 master 的 IP 或主机名。
	Address string `json:"address"`
	// Port 是 apiserver 端口。
	Port int `json:"port"`
}

// LocalProxy 渲染 中 worker 侧的 haproxy。
type LocalProxy struct{}

// NewLocalProxy 返回某个节点的本地代理渲染器。
func NewLocalProxy(c *render.Context) (*LocalProxy, error) {
	if c == nil || c.Cluster == nil {
		return nil, fmt.Errorf("loadbalancer: 缺少集群配置")
	}
	return &LocalProxy{}, nil
}

// Applies 报告本函数是否适用于该节点。
// ：master 直连本机 apiserver，因此本地代理只在 worker 上需要。
// 这里是"按配置里的角色判断"，不是按主机探测。
func (l *LocalProxy) Applies(c *render.Context) bool {
	return c != nil && c.Host != nil && !c.Host.IsMaster() &&
		c.Cluster != nil && c.Cluster.ControlPlane.LocalProxyEnabled()
}

// Backends 按声明顺序返回本节点的 apiserver 端点。
func (l *LocalProxy) Backends(c *render.Context) []Backend {
	if c == nil || c.Cluster == nil {
		return nil
	}
	masters := c.Cluster.Cluster.Hosts.Masters()
	out := make([]Backend, 0, len(masters))
	for _, m := range masters {
		out = append(out, Backend{Name: m.Name, Address: m.Address, Port: constants.APIServerPort})
	}
	return out
}

// Render 返回本组件的渲染结果。
func (l *LocalProxy) Render(c *render.Context, spec *render.Spec) error {
	if !l.Applies(c) {
		return nil
	}
	data, err := l.data(c)
	if err != nil {
		return err
	}
	spec.Add(render.File{
		Path:      c.Paths.Resolve(haproxyConfigPath),
		Content:   []byte(ensureTrailingNewline(data.Config)),
		Mode:      0o644,
		Kind:      render.KindConf,
		Component: localProxyName,
		// 配置变化即 pod 重建，影响只有秒级。
		OnChange: render.ActionRecreate,
	})
	spec.Add(render.File{
		Path:      c.Paths.ManifestPath(localProxyName),
		Content:   []byte(ensureTrailingNewline(data.Manifest)),
		Mode:      0o644,
		Kind:      render.KindManifest,
		Component: localProxyName,
		OnChange:  render.ActionRecreate,
	})
	return nil
}

// localProxyText 汇总本地代理各文件渲染后的文本。
type localProxyText struct {
	Config   string
	Manifest string
}

// data 渲染本节点的配置与清单。
func (l *LocalProxy) data(c *render.Context) (*localProxyText, error) {
	p, err := render.LoadProfile(c)
	if err != nil {
		return nil, err
	}
	image, err := imageFor(c, haproxyBOMComponent)
	if err != nil {
		return nil, err
	}
	data := profile.LocalProxyData{
		Binary:      "haproxy",
		Image:       image,
		Hostname:    c.NodeName(),
		ConfigFile:  c.Paths.Resolve(haproxyConfigPath),
		ProxyPort:   constants.DefaultLocalProxyPort,
		StatsPort:   haproxyStatsPort,
		RestartedAt: "",
		Backends:    l.profileBackends(c),
	}
	config, err := p.RenderTemplate(profile.TmplLocalProxyConfig, data)
	if err != nil {
		return nil, fmt.Errorf("渲染 haproxy 配置失败：%w", err)
	}
	// config-hash 是"等待生效"的判据。后端列表**和渲染后的配置内容**
	// 都必须算进哈希：haproxy 不会监视 hostPath 里的配置文件，只改 haproxy.cfg
	// 而不改清单时，kubelet 不会重建 pod（模板/静态内容变化也会被吞掉）。
	// 把 config 算进来后，清单里的 kubemono.io/config-hash 变化，kubelet 会重建
	data.ConfigHash = fsutil.SHA256Bytes([]byte(l.configHashInput(data) + "\nconfig=" + config))
	manifest, err := p.RenderTemplate(profile.TmplLocalProxyManifest, data)
	if err != nil {
		return nil, fmt.Errorf("渲染 haproxy 清单失败：%w", err)
	}
	return &localProxyText{Config: config, Manifest: manifest}, nil
}

// configHashInput 是本地代理配置的结构化部分，包含后端列表。后端列表必须
// 参与计算：haproxy 只在进程启动时读取配置，因此增删 master 需要新的
// pod/unit，而不只是磁盘上的新文件。
func (l *LocalProxy) configHashInput(data profile.LocalProxyData) string {
	var b strings.Builder
	fmt.Fprintf(&b, "binary=%s\nimage=%s\nconfig=%s\nport=%d\nstats=%d\n",
		data.Binary, data.Image, data.ConfigFile, data.ProxyPort, data.StatsPort)
	for _, backend := range data.Backends {
		fmt.Fprintf(&b, "backend=%s=%s:%d\n", backend.Name, backend.Address, backend.Port)
	}
	return b.String()
}

// profileBackends 把 master 转换成模板用的后端。
func (l *LocalProxy) profileBackends(c *render.Context) []profile.LocalProxyBackend {
	backends := l.Backends(c)
	out := make([]profile.LocalProxyBackend, 0, len(backends))
	for _, b := range backends {
		out = append(out, profile.LocalProxyBackend{Name: b.Name, Address: b.Address, Port: b.Port})
	}
	return out
}

// profileOf 加载当前 release 的 profile。
// ensureTrailingNewline 在缺少结尾换行时补上一个。
func ensureTrailingNewline(s string) string {
	if s == "" || !strings.HasSuffix(s, "\n") {
		return s + "\n"
	}
	return s
}

// LocalProxyConfigPath 返回本地代理的配置路径。
func LocalProxyConfigPath(c *render.Context) string {
	if c == nil {
		return haproxyConfigPath
	}
	return c.Paths.Resolve(haproxyConfigPath)
}

// LocalProxyBackendsFromConfig 从渲染后的 haproxy.cfg 中解析后端列表
// （本地代理阶段用它比较实际与期望，"检查后端列表是否与 desired 一致"）。
func LocalProxyBackendsFromConfig(content string) []Backend {
	var out []Backend
	for line := range strings.SplitSeq(content, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "server ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		host, port, ok := strings.Cut(fields[2], ":")
		if !ok {
			continue
		}
		p, err := strconv.Atoi(port)
		if err != nil {
			continue
		}
		out = append(out, Backend{Name: fields[1], Address: host, Port: p})
	}
	return out
}

// BackendsEqual 按集合比较两个后端列表。
// 比较是顺序无关的：haproxy 的 server 行顺序变化不影响语义，因此不应该被
// 当成"后端列表变了"。
func BackendsEqual(a, b []Backend) bool {
	if len(a) != len(b) {
		return false
	}
	as := backendSet(a)
	bs := backendSet(b)
	if len(as) != len(bs) {
		return false
	}
	for k := range as {
		if !bs[k] {
			return false
		}
	}
	return true
}

func backendSet(in []Backend) map[string]bool {
	out := map[string]bool{}
	for _, b := range in {
		out[fmt.Sprintf("%s=%s:%d", b.Name, b.Address, b.Port)] = true
	}
	return out
}
