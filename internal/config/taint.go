package config

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// ParseTaint 解析 cluster.yaml 里的 taint 声明："key=value:Effect"
// （value 与 effect 可省略，缺省 effect 为 NoSchedule）。
//
// 这是唯一实现：profile、markcontrolplane 与 validate 共用同一套解析，
// 严格解析 + 由调用方决定回退：
//   - config validate 用错误给出可定位的校验信息；
//   - markcontrolplane 用错误拒绝写进 Node；
//   - profile 的模板函数用 LenientTaint（配置已过校验，错误只可能来自
//     未校验路径，回退为 NoSchedule 以免模板渲染中途失败）。
func ParseTaint(raw string) (key, value, effect string, err error) {
	t := strings.TrimSpace(raw)
	if t == "" {
		return "", "", "", fmt.Errorf("taint 不能为空")
	}
	effect = string(corev1.TaintEffectNoSchedule)
	if i := strings.LastIndex(t, ":"); i >= 0 {
		effect = strings.TrimSpace(t[i+1:])
		t = t[:i]
	}
	key, value = t, ""
	if i := strings.Index(t, "="); i >= 0 {
		key, value = strings.TrimSpace(t[:i]), strings.TrimSpace(t[i+1:])
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return "", "", "", fmt.Errorf("taint key 不能为空")
	}
	switch corev1.TaintEffect(effect) {
	case corev1.TaintEffectNoSchedule, corev1.TaintEffectPreferNoSchedule, corev1.TaintEffectNoExecute:
	default:
		return "", "", "", fmt.Errorf("taint effect %q 无效（可选 NoSchedule/PreferNoSchedule/NoExecute）", effect)
	}
	return key, value, effect, nil
}

// LenientTaint 宽松解析：解析失败时回退为 NoSchedule 的空 taint，
// 保持模板函数"永不报错"的契约。正常路径下配置已经过校验，这里只是兜底。
func LenientTaint(raw string) (key, value, effect string) {
	key, value, effect, err := ParseTaint(raw)
	if err != nil {
		return "", "", string(corev1.TaintEffectNoSchedule)
	}
	return key, value, effect
}
