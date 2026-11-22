package config

// JoinSecret 是 join 命令在内存中传递的凭证（不落盘，也不序列化为 YAML）。
// 它由 `kubemono token create` 打印的 join 命令参数构造，含引导令牌；
// master 的还含 certificateKey。两者都不会以明文写入集群。
type JoinSecret struct {
	Role               Role     `json:"role" yaml:"role"`
	Token              string   `json:"token" yaml:"token"`
	CertificateKey     string   `json:"certificateKey,omitempty" yaml:"certificateKey,omitempty"`
	CACertHashes       []string `json:"caCertHashes,omitempty" yaml:"caCertHashes,omitempty"`
	DiscoveryEndpoints []string `json:"discoveryEndpoints,omitempty" yaml:"discoveryEndpoints,omitempty"`
}
