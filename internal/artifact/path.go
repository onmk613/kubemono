package artifact

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// ValidateBundlePath 拒绝绝不应出现在物料内的路径。
// 拒绝：空路径、绝对路径、Windows 盘符路径、反斜杠、NUL、任何 ".." 段，
// 以及非规范的 "./x" 形式。清单与 tar 成员都用它校验，是防目录穿越的第一道闸门。
func ValidateBundlePath(p string) error {
	if p == "" {
		return fmt.Errorf("空路径")
	}
	if strings.ContainsRune(p, '\x00') {
		return fmt.Errorf("路径包含 NUL 字节")
	}
	if strings.Contains(p, `\`) {
		return fmt.Errorf("路径不能包含反斜杠")
	}
	if strings.HasPrefix(p, "/") {
		return fmt.Errorf("不允许绝对路径")
	}
	if len(p) >= 2 && p[1] == ':' {
		return fmt.Errorf("不允许盘符路径")
	}
	for seg := range strings.SplitSeq(p, "/") {
		switch seg {
		case "":
			return fmt.Errorf("路径含空段")
		case ".":
			return fmt.Errorf("路径含 \".\" 段")
		case "..":
			return fmt.Errorf("路径含 \"..\" 段，存在目录穿越风险")
		}
	}
	if cleaned := path.Clean(p); cleaned != p {
		return fmt.Errorf("路径不是规范形式（%s）", cleaned)
	}
	return nil
}

// SafeJoin 把物料内的相对路径拼到 root 下，并再次确认结果没有越界。
func SafeJoin(root, rel string) (string, error) {
	if err := ValidateBundlePath(rel); err != nil {
		return "", err
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("artifact: 解析根目录 %s 失败: %w", root, err)
	}
	dst := filepath.Join(absRoot, filepath.FromSlash(rel))
	absDst, err := filepath.Abs(dst)
	if err != nil {
		return "", fmt.Errorf("artifact: 解析目标路径 %s 失败: %w", dst, err)
	}
	if absDst != absRoot && !strings.HasPrefix(absDst, absRoot+string(os.PathSeparator)) {
		return "", fmt.Errorf("artifact: 路径 %q 越出根目录 %s", rel, root)
	}
	return absDst, nil
}
