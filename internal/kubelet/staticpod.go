package kubelet

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"time"

	"kubemono/internal/constants"
	"kubemono/internal/util/fsutil"

	"go.yaml.in/yaml/v3"
)

// 静态 Pod 清单里 kubemono 使用的注解键。
const (
	AnnotationRestartedAt = "kubemono.io/restarted-at"
)

// BumpStaticPodRestart 更新静态 Pod 清单的 kubemono.io/restarted-at 注解，让 kubelet
// 重建 Pod。它只用于节点级代理（local-proxy），不用于控制面：
// 控制面是 systemd unit。
func BumpStaticPodRestart(paths constants.Paths, component string, now time.Time) error {
	path := paths.ManifestPath(component)
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	updated, err := setAnnotation(data, AnnotationRestartedAt, now.UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("更新 %s 的 %s 注解失败：%w", path, AnnotationRestartedAt, err)
	}
	if err := fsutil.WriteFileAtomic(path, updated, 0o644); err != nil {
		return fmt.Errorf("写入 %s 失败：%w", path, err)
	}
	return nil
}

// setAnnotation 就地替换清单里一个注解的值。
//
// 不做整份 YAML round-trip：drift 比较按行哈希、只豁免 restarted-at 这一行
// （见 render.contentDigestForDrift），整份重编码产生的引号/缩进差异会让清单
// 永久漂移。因此只改目标行，值用 yaml.Marshal 编码——任意字符串都会得到合法
// YAML（多行值编码成块标量，续行按注解层级补缩进）；行匹配用"缩进 + 键名"
// 精确锚定，避免子串误伤（strings.Contains 会把 restarted-at-foo 之类的前缀
// 键也当成目标）。
func setAnnotation(manifest []byte, key, value string) ([]byte, error) {
	encoded, err := yaml.Marshal(value)
	if err != nil {
		return nil, err
	}
	encoded = bytes.TrimSuffix(encoded, []byte("\n"))

	var out bytes.Buffer
	found := false
	for _, line := range strings.Split(string(manifest), "\n") {
		trimmed := strings.TrimLeft(line, " \t")
		indent := line[:len(line)-len(trimmed)]
		if strings.HasPrefix(trimmed, key+":") {
			out.WriteString(indent)
			out.WriteString(key)
			out.WriteString(": ")
			// 块标量（多行值）的续行必须比键多一层缩进才是合法 YAML。
			out.WriteString(strings.ReplaceAll(string(encoded), "\n", "\n"+indent+" "))
			found = true
		} else {
			out.WriteString(line)
		}
		out.WriteString("\n")
	}
	if !found {
		return nil, fmt.Errorf("清单里没有注解 %s", key)
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n")), nil
}
