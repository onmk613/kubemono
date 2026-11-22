package certrenew

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"kubemono/internal/constants"
	"kubemono/internal/util/fsutil"
)

// pendingFile 记录“证书已重签、但组件还没成功重启”。
// 与 certs phase 的 cert-restart.json 同构：没有它，重启失败后重跑
// `cert renew` 会因为证书已经不在续期窗口而跳过，旧进程一直用旧证书。
type pendingFile struct {
	Components []string `json:"components"`
}

func pendingPath(paths constants.Paths) string {
	return filepath.Join(paths.State(), "cert-renew-pending.json")
}

func readPending(paths constants.Paths) ([]string, error) {
	data, err := os.ReadFile(pendingPath(paths))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var p pendingFile
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("解析证书待重启标记 %s 失败: %w", pendingPath(paths), err)
	}
	return p.Components, nil
}

// addPending 把组件并入待重启标记（先落盘，再写证书）。
func addPending(paths constants.Paths, components []string) error {
	if len(components) == 0 {
		return nil
	}
	existing, err := readPending(paths)
	if err != nil {
		return err
	}
	set := map[string]bool{}
	for _, c := range existing {
		set[c] = true
	}
	for _, c := range components {
		set[c] = true
	}
	all := make([]string, 0, len(set))
	for c := range set {
		all = append(all, c)
	}
	sort.Strings(all)
	data, err := json.MarshalIndent(pendingFile{Components: all}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(pendingPath(paths)), 0o700); err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(pendingPath(paths), append(data, '\n'), 0o600)
}

func clearPending(paths constants.Paths) error {
	if err := os.Remove(pendingPath(paths)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
