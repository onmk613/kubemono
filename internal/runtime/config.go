package runtime

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"kubemono/internal/constants"
	"kubemono/internal/render"
)

// ConfigDecision 描述受管配置变更后运行时需要执行的动作。
type ConfigDecision string

// 动作分级。
const (
	// DecisionNone 没有配置变更，不做任何动作。
	DecisionNone ConfigDecision = "none"
	// DecisionReload：只有 certs.d（镜像仓库）变化，containerd 热加载，
	// 不执行任何 systemd 动作。
	DecisionReload ConfigDecision = "reload"
	// DecisionRestart config.toml 或其它受管文件变化：必须重启运行时。
	DecisionRestart ConfigDecision = "restart"
)

// Plan 是一次 ApplyConfig 的结果，供 phase 记录动作与回滚。
type Plan struct {
	// Changes 是本次的差异；为空表示内容未变。
	Changes []render.FileChange
	// Applied 是实际落盘的变更（dry-run 时为空）。
	Applied []render.AppliedChange
	// Decision 是重新加载/重启判定。
	Decision ConfigDecision
	// BackupDir 是变更前备份目录。
	BackupDir string
	// Restarted 表示确实调用了服务管理器的 restart。
	Restarted bool
}

// configPath 返回受管的 containerd 配置路径。
func configPath(paths constants.Paths) string {
	return render.ContainerdConfigPath(&render.Context{Paths: paths})
}

// certsDir 返回受管的 certs.d 目录。
func certsDir(paths constants.Paths) string {
	return render.ContainerdCertsDir(&render.Context{Paths: paths})
}

// ClassifyChanges 返回变更对应的动作决策。
// 判定规则（唯一规则，测试与 phase 共用）：
// - 没有变更 → none；
// - 变更里出现 config.toml → restart；
// - 变更里出现 certs.d 之外的任何路径 → restart（保守，宁多一次重启）；
// - 其余（只有 certs.d 下的文件） → reload（containerd 热加载，不重启）。
func ClassifyChanges(config string, certs string, changes []render.FileChange) ConfigDecision {
	if len(changes) == 0 {
		return DecisionNone
	}
	prefix := strings.TrimSuffix(certs, "/") + "/"
	for _, ch := range changes {
		path := ch.File.Path
		if path == config {
			return DecisionRestart
		}
		if strings.HasPrefix(path, prefix) {
			continue
		}
		return DecisionRestart
	}
	return DecisionReload
}

// ManagedFiles 返回渲染结果中由 containerd 托管的文件（config.toml + certs.d）。
func ManagedFiles(spec *render.Spec) []render.File {
	if spec == nil {
		return nil
	}
	out := make([]render.File, 0, len(spec.Files))
	for _, f := range spec.Files {
		if f.Component == ComponentContainerd {
			out = append(out, f)
		}
	}
	return out
}

// managedSpec 只保留 containerd 的受管文件，避免把其它组件的差异算进来。
func managedSpec(spec *render.Spec) *render.Spec {
	out := &render.Spec{}
	if spec == nil {
		return out
	}
	for _, f := range ManagedFiles(spec) {
		out.Add(f)
	}
	out.Sort()
	return out
}

// fileMode 返回普通文件的权限位（不含类型位）。
func fileMode(path string) (uint32, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, fmt.Errorf("读取 %s 权限失败：%w", path, err)
	}
	return uint32(info.Mode().Perm()), nil
}

// ConfigHash 计算一组受管文件的稳定摘要（路径 + 权限 + 内容）。
func ConfigHash(files []render.File) string {
	spec := &render.Spec{Files: append([]render.File(nil), files...)}
	spec.Sort()
	return spec.Hash()
}

// DesiredConfigHash 返回渲染结果中受管配置的摘要；custom 运行时返回空串。
func DesiredConfigHash(spec *render.Spec) string {
	files := ManagedFiles(spec)
	if len(files) == 0 {
		return ""
	}
	return ConfigHash(files)
}

// LiveSpec 读取磁盘上受管的 containerd 配置（config.toml + certs.d）。
// 权限位必须记录**磁盘上的真实值**：渲染侧 certs.d/hosts.toml 是 0600
// （含 registry 凭据），如果这里硬编码 0644，ComputeChanges 每次都会判成
// “权限漂移”，runtime phase 永远不收敛，且每次 repair 都多一份备份。
func LiveSpec(paths constants.Paths) (*render.Spec, error) {
	spec := &render.Spec{}
	cfg := configPath(paths)
	switch data, err := os.ReadFile(cfg); {
	case err == nil:
		mode, statErr := fileMode(cfg)
		if statErr != nil {
			return nil, statErr
		}
		spec.Add(render.File{
			Path: cfg, Content: data, Mode: mode,
			Component: ComponentContainerd, Kind: render.KindConf, OnChange: render.ActionRestart,
		})
	case os.IsNotExist(err):
	default:
		return nil, fmt.Errorf("读取 %s 失败：%w", cfg, err)
	}

	dir := certsDir(paths)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		mode, merr := fileMode(p)
		if merr != nil {
			return merr
		}
		spec.Add(render.File{
			Path: p, Content: data, Mode: mode,
			Component: ComponentContainerd, Kind: render.KindConf, OnChange: render.ActionNone,
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("遍历 %s 失败：%w", dir, err)
	}
	spec.Sort()
	return spec, nil
}
