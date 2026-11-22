package config

// NodeSpec 汇总所有节点共用的主机、运行时与 kubelet 设置。
type NodeSpec struct {
	Host    HostInitSpec `json:"host,omitempty" yaml:"host,omitempty"`
	Runtime RuntimeSpec  `json:"runtime,omitempty" yaml:"runtime,omitempty"`
	Kubelet KubeletSpec  `json:"kubelet,omitempty" yaml:"kubelet,omitempty"`
}

// HostInitSpec 配置主机层面的初始化。
type HostInitSpec struct {
	KernelModules []string          `json:"kernelModules,omitempty" yaml:"kernelModules,omitempty"`
	Sysctls       map[string]string `json:"sysctls,omitempty" yaml:"sysctls,omitempty"`
}

// RuntimeSpec 配置容器运行时。
// 没有 type：custom 任一字段非空即视为 custom，否则默认 containerd。
type RuntimeSpec struct {
	Containerd ContainerdSpec    `json:"containerd,omitempty" yaml:"containerd,omitempty"`
	Custom     CustomRuntimeSpec `json:"custom,omitempty" yaml:"custom,omitempty"`
}

// IsCustom 报告是否使用用户自备的 CRI。
func (s RuntimeSpec) IsCustom() bool { return s.Custom.Configured() }

// Mode 返回 containerd 或 custom，用于状态/选择物料。
func (s RuntimeSpec) Mode() string {
	if s.IsCustom() {
		return RuntimeCustom
	}
	return RuntimeContainerd
}

// ContainerdSpec 配置托管的 containerd。
type ContainerdSpec struct {
	Root          string         `json:"root,omitempty" yaml:"root,omitempty"`
	SystemdCgroup *bool          `json:"systemdCgroup,omitempty" yaml:"systemdCgroup,omitempty"`
	Registries    RegistriesSpec `json:"registries,omitempty" yaml:"registries,omitempty"`
	ConfigPatch   string         `json:"configPatch,omitempty" yaml:"configPatch,omitempty"`
}

// RegistriesSpec 是镜像仓库 mirror 与认证配置。
type RegistriesSpec struct {
	Mirrors map[string][]string         `json:"mirrors,omitempty" yaml:"mirrors,omitempty"`
	Configs map[string]RegistryAuthSpec `json:"configs,omitempty" yaml:"configs,omitempty"`
}

// RegistryAuthSpec 配置单个镜像仓库端点。
type RegistryAuthSpec struct {
	CAFile             string `json:"caFile,omitempty" yaml:"caFile,omitempty"`
	InsecureSkipVerify bool   `json:"insecureSkipVerify,omitempty" yaml:"insecureSkipVerify,omitempty"`
	Username           string `json:"username,omitempty" yaml:"username,omitempty"`
	Password           string `json:"password,omitempty" yaml:"password,omitempty"`
}

// CustomRuntimeSpec 是用户自备的 CRI。
type CustomRuntimeSpec struct {
	CRISocket    string `json:"criSocket,omitempty" yaml:"criSocket,omitempty"`
	CgroupDriver string `json:"cgroupDriver,omitempty" yaml:"cgroupDriver,omitempty"`
	ServiceName  string `json:"serviceName,omitempty" yaml:"serviceName,omitempty"`
}

// Configured 报告用户是否填写了 custom 运行时字段。
func (s CustomRuntimeSpec) Configured() bool {
	return s.CRISocket != "" || s.CgroupDriver != "" || s.ServiceName != ""
}

// KubeletSpec 配置 kubelet。
type KubeletSpec struct {
	// RootDir 是 kubelet 数据目录（--root-dir）。
	RootDir string `json:"rootDir,omitempty" yaml:"rootDir,omitempty"`
	// ConfigPatch 覆盖 kubelet 配置文件里的字段。
	ConfigPatch map[string]any `json:"configPatch,omitempty" yaml:"configPatch,omitempty"`
	// ExtraArgs 透传 kubelet CLI flag。
	ExtraArgs map[string]string `json:"extraArgs,omitempty" yaml:"extraArgs,omitempty"`
}
