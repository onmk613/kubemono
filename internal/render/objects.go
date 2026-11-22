package render

import (
	"fmt"

	"kubemono/internal/util/fsutil"

	"sigs.k8s.io/yaml"
)

// Object 是组件必须通过 apiserver 创建的集群级对象
// （例如 kube-router 的 DaemonSet/ConfigMap/SA/RBAC）。
//
// 为什么和 File 分开：File 是"主机上的期望状态"，由 render.Apply 原子落盘；
// Object 是"集群里的期望状态"，只能经过 apiserver 创建。两者职责不同，
// 混在一个 Spec 里会让漂移比较同时面对磁盘与 apiserver，难以解释。
type Object interface {
	// Ref 是 "kind/namespace/name"（集群级对象省略 namespace）。
	Ref() string
	// Kind 是对象类型，例如 "DaemonSet"。
	Kind() string
	// Name 是对象名称。
	Name() string
	// Namespace 是命名空间；集群级对象为空。
	Namespace() string
	// Data 将对象序列化为 YAML（单文档、确定性）。
	Data() ([]byte, error)
}

// ClusterObject 包装 Kubernetes API 对象，使其满足 Object。
// 用包装类型而不是在每个 Kubernetes 类型上实现方法：一是 corev1.Pod 之类的类型
// 已经有 APIVersion 字段（与方法同名会冲突），二是包装类型让"哪些字段参与
// 幂等比对"显式可控（fingerprint 只取 metadata+spec，避免 status 噪声）。
type ClusterObject struct {
	// Resource 是 Kind，例如 "DaemonSet"。
	Resource string
	// NS 是命名空间；集群级对象为空。
	NS string
	// ObjectName 是对象名称。
	ObjectName string
	// Value 是有类型的 Kubernetes 对象。
	Value any
}

// Ref 实现 Object。
func (o ClusterObject) Ref() string {
	if o.NS != "" {
		return fmt.Sprintf("%s/%s/%s", o.Resource, o.NS, o.ObjectName)
	}
	return fmt.Sprintf("%s/%s", o.Resource, o.ObjectName)
}

// Kind 实现 Object。
func (o ClusterObject) Kind() string { return o.Resource }

// Name 实现 Object。
func (o ClusterObject) Name() string { return o.ObjectName }

// Namespace 实现 Object。
func (o ClusterObject) Namespace() string { return o.NS }

// Data 实现 Object。
// 序列化会丢掉 status 与 creationTimestamp：客户端构造的对象本来就没有这些值，
// 留着它们只会让 YAML 里出现 `status: {}` 之类的噪声，并让"内容是否变化"的
// 判断不稳定。先序列化再反序列化一次即可得到干净的声明式文档。
func (o ClusterObject) Data() ([]byte, error) {
	if o.Value == nil {
		return nil, fmt.Errorf("render: %s 没有内容", o.Ref())
	}
	raw, err := yaml.Marshal(o.Value)
	if err != nil {
		return nil, fmt.Errorf("render: 序列化 %s 失败: %w", o.Ref(), err)
	}
	var generic map[string]any
	if err := yaml.Unmarshal(raw, &generic); err != nil {
		return nil, fmt.Errorf("render: 归一化 %s 失败: %w", o.Ref(), err)
	}
	delete(generic, "status")
	if meta, ok := generic["metadata"].(map[string]any); ok {
		delete(meta, "creationTimestamp")
	}
	data, err := yaml.Marshal(generic)
	if err != nil {
		return nil, fmt.Errorf("render: 序列化 %s 失败: %w", o.Ref(), err)
	}
	return fsutil.EnsureTrailingNewline(data), nil
}

// NewClusterObject 包装有类型的 Kubernetes 对象。
func NewClusterObject(kind, namespace, name string, value any) *ClusterObject {
	return &ClusterObject{Resource: kind, NS: namespace, ObjectName: name, Value: value}
}

// ObjectSet 是有序的集群级对象集合。
// 顺序即创建顺序：先建 RBAC 再建工作负载；删除时调用方逆序处理。
type ObjectSet struct {
	objects []Object
}

// Add 追加对象。nil 对象会被忽略，调用方无需处处判空即可按条件构建集合。
func (s *ObjectSet) Add(objs ...Object) {
	for _, o := range objs {
		if o == nil {
			continue
		}
		s.objects = append(s.objects, o)
	}
}

// Objects 按插入顺序返回对象。
func (s *ObjectSet) Objects() []Object {
	if s == nil {
		return nil
	}
	return s.objects
}

// Len 返回对象数量。
func (s *ObjectSet) Len() int {
	if s == nil {
		return 0
	}
	return len(s.objects)
}

// Refs 按顺序返回每个对象的稳定引用。
func (s *ObjectSet) Refs() []string {
	out := make([]string, 0, s.Len())
	for _, o := range s.objects {
		out = append(out, o.Ref())
	}
	return out
}

// Find 返回指定 kind 和 name 的对象。
func (s *ObjectSet) Find(kind, name string) (Object, bool) {
	for _, o := range s.objects {
		if o.Kind() == kind && o.Name() == name {
			return o, true
		}
	}
	return nil, false
}
