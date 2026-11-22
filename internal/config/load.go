package config

import (
	"bytes"
	"fmt"

	yamlv3 "go.yaml.in/yaml/v3"
	"sigs.k8s.io/yaml"
)

// ValidateClusterDocument 严格解析 cluster.yaml 并返回聚合校验结果。
// 与 LoadCluster 的差异：不短路，一次给出全部错误。
func ValidateClusterDocument(data []byte) (*Cluster, ValidationResult, error) {
	c := &Cluster{}
	if err := yaml.UnmarshalStrict(data, c); err != nil {
		return nil, ValidationResult{}, fmt.Errorf("parse cluster configuration: %w", err)
	}
	SetDefaults(c)
	v := Validate(c)
	return c, v, nil
}

// LoadCluster 解码、填充默认值并校验 cluster.yaml 文档。
// 未知字段会被拒绝：配置里写错字段名是运维事故的常见来源。
func LoadCluster(data []byte) (*Cluster, error) {
	c := &Cluster{}
	if err := yaml.UnmarshalStrict(data, c); err != nil {
		return nil, fmt.Errorf("parse cluster configuration: %w", err)
	}
	SetDefaults(c)
	res := Validate(c)
	if err := res.Err(); err != nil {
		return nil, err
	}
	c.Warnings = append([]string(nil), res.Warnings...)
	return c, nil
}

// MarshalCluster 把集群配置序列化为 YAML。
func MarshalCluster(c *Cluster) ([]byte, error) {
	return marshalYAML(c)
}

// marshalYAML 按结构体字段顺序输出 YAML（2 空格缩进），而不是按 key 排序。
// sigs.k8s.io/yaml 走 JSON 再转 YAML，顶层与嵌套字段都会变成字母序；配置模板
// 与写入 Secret 的 cluster.yaml 需要保持各命名段的阅读顺序。
// 反序列化仍由 sigs.k8s.io/yaml 完成，yaml tag 与 json tag 一一对应。
func marshalYAML(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := yamlv3.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
