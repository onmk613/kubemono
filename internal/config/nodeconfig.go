package config

import (
	"fmt"
	"time"

	"sigs.k8s.io/yaml"
)

// NodeConfig 是已安装节点的本地身份（/usr/local/kubemono/node.yaml）。
type NodeConfig struct {
	ClusterID string `json:"clusterID" yaml:"clusterID"`
	// HostName 是节点在 cluster.yaml 中的身份键（Host.Name）。
	HostName    string    `json:"hostName,omitempty" yaml:"hostName,omitempty"`
	NodeName    string    `json:"nodeName" yaml:"nodeName"`
	Address     string    `json:"address" yaml:"address"`
	Roles       []Role    `json:"roles" yaml:"roles"`
	Release     string    `json:"release" yaml:"release"`
	InstalledAt time.Time `json:"installedAt,omitempty" yaml:"installedAt,omitempty"`
	UpdatedAt   time.Time `json:"updatedAt,omitempty" yaml:"updatedAt,omitempty"`
}

// LoadNodeConfig 解码 /usr/local/kubemono/node.yaml。
// 严格解析：未知字段直接拒绝。
func LoadNodeConfig(data []byte) (*NodeConfig, error) {
	n := &NodeConfig{}
	if err := yaml.UnmarshalStrict(data, n); err != nil {
		return nil, fmt.Errorf("parse node config: %w", err)
	}
	if n.NodeName == "" {
		return nil, fmt.Errorf("node config is missing nodeName")
	}
	if n.HostName == "" {
		return nil, fmt.Errorf("node config is missing hostName")
	}
	return n, nil
}

// MarshalNodeConfig 序列化节点身份文档。
func MarshalNodeConfig(n *NodeConfig) ([]byte, error) {
	return marshalYAML(n)
}
