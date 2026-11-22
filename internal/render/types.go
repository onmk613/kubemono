// Package render 把（集群配置 + 节点身份）转换成节点必须具备的一组精确文件。
// 设计约束：
//   - 只有一个生成器：install 与 node repair 都调用同一套 Builder，
//     因此不存在“修复后的和安装时写下的不一样”的情况；
//   - 渲染结果先比较再落盘：内容未变就不写、不重启；
//   - 所有内容都是纯数据：渲染过程不访问 apiserver，也不读取主机状态，
//
// 因此可以在准备机上离线生成任意节点的 desired。
package render

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"sort"
	"strings"

	"kubemono/internal/bom"
	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	"kubemono/internal/util/fsutil"
)

// Kind 分类一个渲染结果文件。
type Kind string

// 文件类型取值。
const (
	KindManifest   Kind = "manifest"   // 静态 Pod 清单 / static pod manifest
	KindUnit       Kind = "unit"       // systemd 单元 / systemd unit
	KindConfig     Kind = "config"     // 组件配置文件 / component configuration file
	KindKubeconfig Kind = "kubeconfig" // kubeconfig 文件 / kubeconfig
	KindConf       Kind = "conf"       // 其他配置（haproxy、containerd 等）/ other configuration (haproxy, containerd, ...)
	KindOther      Kind = "other"
)

// Action 告诉应用器文件内容变化后该做什么。
type Action string

// 变更动作取值。
const (
	// ActionNone 表示文件会被自动读取（例如 certs.d）。
	ActionNone Action = "none"
	// ActionReload 表示组件重新加载配置（SIGHUP / reload）。
	ActionReload Action = "reload"
	// ActionRestart 表示组件必须重启。
	ActionRestart Action = "restart"
	// ActionRecreate 表示必须重建 pod/unit（static pod manifest）。
	ActionRecreate Action = "recreate"
)

// File 是一个渲染出来的文件。
type File struct {
	// Path 是目标绝对路径（已带 constants.Paths.Root 前缀）。
	Path string
	// Content 是完整文件内容。
	Content []byte
	// Mode 是文件权限；机密文件必须为 0600。
	Mode uint32
	// Kind 是文件分类。
	Kind Kind
	// Component 是文件所属组件（kube-apiserver、kubelet、containerd 等）。
	Component string
	// OnChange 是内容变化后应用器必须执行的动作。
	OnChange Action
	// Secret 标记包含密钥材料的文件（强制 0600）。
	Secret bool
}

// SHA256 返回文件内容的摘要。
func (f File) SHA256() string { return fsutil.SHA256Bytes(f.Content) }

// contentDigestForDrift 在哈希内容时忽略 kubemono.io/restarted-at。
// 这个 annotation 是 controlplane 重启 static pod 的机制（每次重启写一个新时间），
// 不属于“配置漂移”。如果 Drift/ComputeChanges 把它算进哈希，fileDrift guard 会
// 每 30 秒把 desired 里的旧时间写回去，导致 apiserver 反复重启——真机实测会把
// 控制面打进“刚起来又被改写”的循环。
func contentDigestForDrift(content []byte) string {
	if !bytes.Contains(content, []byte("kubemono.io/restarted-at:")) {
		return fsutil.SHA256Bytes(content)
	}
	var b bytes.Buffer
	for _, line := range bytes.SplitAfter(content, []byte("\n")) {
		if bytes.Contains(line, []byte("kubemono.io/restarted-at:")) {
			continue
		}
		b.Write(line)
	}
	return fsutil.SHA256Bytes(b.Bytes())
}

// RelPath 返回相对 root 前缀的路径，用于 desired/ 镜像。
func (f File) RelPath(root string) string {
	if root == "" {
		return strings.TrimPrefix(f.Path, "/")
	}
	rel := strings.TrimPrefix(f.Path, root)
	return strings.TrimPrefix(rel, "/")
}

// Spec 是一个节点完整的 desired 状态。
type Spec struct {
	// Node 是生成该 spec 的节点名。
	Node string
	// ClusterID 是生成该 spec 的集群身份。
	ClusterID string
	// Release 是产出该 spec 的 kubemono 版本。
	Release string
	// Files 是渲染出的文件，按路径排序。
	Files []File
}

// Add 追加一个文件。重复路径由 Validate 拒绝。
func (s *Spec) Add(f File) {
	if f.Mode == 0 {
		f.Mode = 0o644
	}
	if f.Secret && f.Mode&0o077 != 0 {
		f.Mode = 0o600
	}
	s.Files = append(s.Files, f)
}

// AddFile 是文本文件的便捷封装。
func (s *Spec) AddFile(path string, content []byte, mode uint32, component string, kind Kind, onChange Action) {
	s.Add(File{Path: path, Content: content, Mode: mode, Component: component, Kind: kind, OnChange: onChange})
}

// Sort 按路径排序文件，使下游所有比较与 golden 测试都是确定的。
func (s *Spec) Sort() {
	sort.Slice(s.Files, func(i, j int) bool { return s.Files[i].Path < s.Files[j].Path })
}

// Validate 检查那些否则会造成静默损坏的不变量。
func (s *Spec) Validate() error {
	seen := map[string]bool{}
	for _, f := range s.Files {
		switch {
		case f.Path == "":
			return fmt.Errorf("render: file with empty path (component %s)", f.Component)
		case !strings.HasPrefix(f.Path, "/"):
			return fmt.Errorf("render: path %q must be absolute", f.Path)
		case strings.Contains(f.Path, ".."):
			return fmt.Errorf("render: path %q must not contain '..'", f.Path)
		case path.Clean(f.Path) != f.Path:
			return fmt.Errorf("render: path %q is not clean", f.Path)
		}
		if seen[f.Path] {
			return fmt.Errorf("render: %s is rendered twice", f.Path)
		}
		seen[f.Path] = true
		if f.Component == "" {
			return fmt.Errorf("render: file %s has no owning component", f.Path)
		}
		if f.Secret && f.Mode&0o077 != 0 {
			return fmt.Errorf("render: secret file %s must not be group/world accessible (mode %04o)", f.Path, f.Mode)
		}
	}
	return nil
}

// Hash 返回整个 spec 的单个摘要（只含内容，不含时间戳）。
// 用途：判断“本机 desired 是否需要重新应用”、以及在节点状态里上报一个短标识。
func (s *Spec) Hash() string {
	h := sha256.New()
	files := make([]File, len(s.Files))
	copy(files, s.Files)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	for _, f := range files {
		_, _ = fmt.Fprintf(h, "%s\x00%04o\x00%s\x00%d\x00", f.Path, f.Mode, f.SHA256(), len(f.Content))
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// Find 返回指定路径的文件。
func (s *Spec) Find(path string) (File, bool) {
	for _, f := range s.Files {
		if f.Path == path {
			return f, true
		}
	}
	return File{}, false
}

// Components 返回至少拥有一个文件的组件。
func (s *Spec) Components() []string {
	set := map[string]bool{}
	for _, f := range s.Files {
		set[f.Component] = true
	}
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// FilesOf 返回某个组件的文件。
func (s *Spec) FilesOf(component string) []File {
	var out []File
	for _, f := range s.Files {
		if f.Component == component {
			out = append(out, f)
		}
	}
	return out
}

// Builder 是每个渲染器都要实现的接口。
// 一个 Builder 只负责一个组件；Spec 的组装由 internal/render 的 BuilderSet 完成。
type Builder interface {
	// Name 是该 builder 渲染的组件。
	Name() string
	// Applies 报告该组件是否存在于本节点。
	Applies(c *Context) bool
	// Render 把它的文件追加到 spec。
	Render(c *Context, spec *Spec) error
}

// Context 携带渲染器需要的信息；它刻意不包含任何 Kubernetes 客户端，
// 以保证渲染离线且确定。
type Context struct {
	// Cluster 是期望的集群配置（已填默认值并校验）。
	Cluster *clusterconfig.Cluster
	// Host 是该节点在配置中的条目。
	Host *clusterconfig.Host
	// IsInitNode 表示本机本次执行显式的 kubemono init（stacked etcd 用 new）。
	IsInitNode bool
	// NodeNameValue 是本机实际的 Kubernetes NodeName；为空时按 Node/Host 推导。
	NodeNameValue string
	// Node 是已安装的节点身份；安装前可能为 nil，此时渲染器用 Host.NodeNameOrName
	// 作为节点名。
	Node *clusterconfig.NodeConfig
	// BOM 是内嵌的物料清单（bill of materials）。
	BOM *bom.BOM
	// Paths 解析所有目标路径（生产中 Root 为空）。
	Paths constants.Paths
	// Release 是正在渲染的 kubemono 版本。
	Release string
	// ClusterID 是不可变的集群身份（init 之前为空）。
	ClusterID string
	// EtcdInitialCluster 覆盖渲染时的 --initial-cluster；为空时按集群配置生成。
	EtcdInitialCluster []string
	// EtcdInitialClusterState 对 init 节点是 "new"，对加入已运行集群的 master
	// 是 "existing"；为空表示 "new"。
	EtcdInitialClusterState string
	// HostMTU 是本机目标网卡 MTU；0 表示使用 DefaultHostMTU（1500）。
	// install / node repair 在 Linux 上会探测，未检测到时为 0。
	HostMTU int
}

// NodeName 返回正在渲染的节点名。
func (c *Context) NodeName() string {
	return clusterconfig.NodeNameOf(c.NodeNameValue, c.Node, c.Host)
}

// IsMaster 报告该节点是否运行控制面。
func (c *Context) IsMaster() bool { return c.Host != nil && c.Host.IsMaster() }

// Image 返回组件必须使用的完整镜像引用。
// 镜像地址来自生效 BOM（含 cluster.yaml 的按组件覆盖），没有全局仓库前缀。
func (c *Context) Image(component string) (string, error) {
	if c.BOM == nil {
		return "", fmt.Errorf("render: BOM is not set in the render context")
	}
	img, err := c.BOM.Image(component)
	if err != nil {
		return "", err
	}
	return img.Reference(), nil
}

// Render 按注册顺序运行每个适用的 builder，返回已排序并通过校验的 spec。
func Render(c *Context, builders ...Builder) (*Spec, error) {
	spec := &Spec{
		Node:      c.NodeName(),
		ClusterID: c.ClusterID,
		Release:   c.Release,
	}
	for _, b := range builders {
		if b == nil || !b.Applies(c) {
			continue
		}
		if err := b.Render(c, spec); err != nil {
			return nil, fmt.Errorf("render %s: %w", b.Name(), err)
		}
	}
	spec.Sort()
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	return spec, nil
}
