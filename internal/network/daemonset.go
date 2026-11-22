package network

import (
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/yaml"
)

// configHashAnnotation 是 kubemono 写入每个 Pod 模板的注解，让配置变更触发工作负载
// 滚动更新（与 是同一条约定）。
const configHashAnnotation = "kubemono.io/config-hash"

// newDaemonSet 构建一个使用 RollingUpdate 策略的 DaemonSet，同一时刻最多允许
// maxUnavailable 个 Pod 不可用。
func newDaemonSet(name string, labels map[string]string, podSpec corev1.PodSpec, hash string, maxUnavailable int) *appsv1.DaemonSet {
	unavailable := intstr.FromInt32(int32(maxUnavailable))
	return &appsv1.DaemonSet{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSet"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kube-system", Labels: labels},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			UpdateStrategy: appsv1.DaemonSetUpdateStrategy{
				Type:          appsv1.RollingUpdateDaemonSetStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDaemonSet{MaxUnavailable: &unavailable},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels:      labels,
					Annotations: map[string]string{configHashAnnotation: hash},
				},
				Spec: podSpec,
			},
		},
	}
}

// parsePodSpec 把 .podspec 模板渲染出的 YAML 解析成 PodSpec。
// 模板只描述 Pod 的部分字段（没有 apiVersion/kind），因此直接解析成 PodSpec：
// DaemonSet 的 PodTemplate 与静态 Pod 清单共用同一份定义。
func parsePodSpec(rendered string) (corev1.PodSpec, error) {
	// .podspec 模板只描述 PodSpec 本身（没有 apiVersion/kind），因此直接解析成
	// corev1.PodSpec：sigs.k8s.io/yaml 先转 JSON 再按 json tag 匹配，"spec:" 不是
	// 这层数据的顶层字段。
	var podSpec corev1.PodSpec
	if err := yaml.Unmarshal([]byte(rendered), &podSpec); err != nil {
		return corev1.PodSpec{}, fmt.Errorf("解析 Pod 定义失败：%w", err)
	}
	if len(podSpec.Containers) == 0 {
		return corev1.PodSpec{}, fmt.Errorf("pod 定义里没有容器：模板渲染结果为空")
	}
	if !podSpec.HostNetwork {
		return corev1.PodSpec{}, fmt.Errorf("pod 定义必须使用 hostNetwork")
	}
	return podSpec, nil
}
