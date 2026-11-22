package bom

import (
	"fmt"
	"strconv"
	"strings"
)

// Release 是一个 kubemono 发行版本：v<k8s>-km.<n>。
// 例：v1.31.4-km.1 → Kubernetes=v1.31.4, Patch=1。
type Release struct {
	// Kubernetes 是上游 Kubernetes 版本，例如 "v1.31.4"。
	Kubernetes string
	// Patch 是 kubemono patch 号（km.N 中的 N）。0 表示该版本不是 kubemono 发行版
	// （纯上游版本）。
	Patch int
	// Raw 是原始字符串。
	Raw string
}

// ParseRelease 解析 kubemono 发行版本字符串。
func ParseRelease(s string) (Release, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return Release{}, fmt.Errorf("empty release version")
	}
	r := Release{Raw: raw}
	base := raw
	if strings.Contains(raw, "-km") && !strings.Contains(raw, "-km.") {
		return Release{}, fmt.Errorf("invalid kubemono release %q: expected -km.<n>", raw)
	}
	if before, after, ok := strings.Cut(raw, "-km."); ok {
		base = before
		n, err := strconv.Atoi(after)
		if err != nil || n < 1 {
			return Release{}, fmt.Errorf("invalid kubemono release %q: patch must be a positive integer", raw)
		}
		r.Patch = n
	}
	if !strings.HasPrefix(base, "v") {
		return Release{}, fmt.Errorf("invalid release %q: must start with 'v'", raw)
	}
	if err := validateSemver(base); err != nil {
		return Release{}, fmt.Errorf("invalid release %q: %w", raw, err)
	}
	r.Kubernetes = base
	return r, nil
}

// String 实现 fmt.Stringer。
func (r Release) String() string {
	if r.Patch > 0 {
		return fmt.Sprintf("%s-km.%d", r.Kubernetes, r.Patch)
	}
	return r.Kubernetes
}

// validateSemver 校验 "v<major>.<minor>.<patch>[-<prerelease>][+<build>]"。
// Kubernetes 的正式版本总是三段；不接受 v1.31 这种两段写法。空段、非数字、
// 前导零一律拒绝，避免 "v1.031" 与 "v1.31" 被当成同一版本。只做校验、不返回
// 解析结果：kubemono 不比较版本（"任意 k8s 版本都允许"），这里唯一要保证的
// 是字符串本身合法。
func validateSemver(v string) error {
	raw := strings.TrimSpace(v)
	s := strings.TrimPrefix(raw, "v")
	// build metadata 不参与校验。
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	if i := strings.IndexByte(s, '-'); i >= 0 {
		for _, id := range strings.Split(s[i+1:], ".") {
			if id == "" {
				return fmt.Errorf("empty prerelease identifier in %q", v)
			}
		}
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return fmt.Errorf("expected major.minor.patch, got %q", v)
	}
	for _, part := range parts {
		if part == "" {
			return fmt.Errorf("empty numeric segment in %q", v)
		}
		if len(part) > 1 && part[0] == '0' {
			return fmt.Errorf("numeric segment %q has a leading zero in %q", part, v)
		}
		if n, err := strconv.Atoi(part); err != nil || n < 0 {
			return fmt.Errorf("invalid numeric segment %q in %q", part, v)
		}
	}
	return nil
}
