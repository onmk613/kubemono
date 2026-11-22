package config

// AddonsSpec 配置内置附加组件。
type AddonsSpec struct {
	CoreDNS CoreDNSSpec `json:"coredns,omitempty" yaml:"coredns,omitempty"`
}

// CoreDNSSpec 配置 coredns 附加组件。
// CoreDNS 的配置主体是 Corefile，用户扩展放在受管区块之外；这里只暴露两个
// 进程级、且确实只能通过 CLI 设置的开关，不开放通用 extraArgs。
type CoreDNSSpec struct {
	Enabled  *bool    `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	Replicas *int32   `json:"replicas,omitempty" yaml:"replicas,omitempty"`
	Upstream []string `json:"upstream,omitempty" yaml:"upstream,omitempty"`
	Hosts    []string `json:"hosts,omitempty" yaml:"hosts,omitempty"`
	// LogFormat 是 CoreDNS 进程日志格式：text（默认）或 json。
	LogFormat string `json:"logFormat,omitempty" yaml:"logFormat,omitempty"`
	// Quiet 关闭 CoreDNS 启动时的版本/初始化输出。
	Quiet bool `json:"quiet,omitempty" yaml:"quiet,omitempty"`
}
