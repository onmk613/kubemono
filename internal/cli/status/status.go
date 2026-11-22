// Package status 实现 `kubemono status`：只读汇总本机身份、收敛状态、
// 证书、systemd unit 与 Node Ready。无守护进程模型下，这是排障的“眼睛”。
package status

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"kubemono/internal/bom"
	"kubemono/internal/certrenew"
	"kubemono/internal/cli/options"
	"kubemono/internal/cli/term"
	"kubemono/internal/cli/utils"
	clusterconfig "kubemono/internal/config"
	"kubemono/internal/identity"
	"kubemono/internal/phase"
	"kubemono/internal/render"
	"kubemono/internal/systemd"
)

const statusLong = `只读汇总本机状态：身份、release、desired 快照漂移、phase 最近结果、
待重启标记、证书到期、systemd unit 与 Kubernetes Node Ready。

不修改任何文件，也不访问 apiserver（除非 --kubeconfig 可用时查询 Node）。
用于替代“翻 state/*.json + systemctl + ls 证书”的手工排障。`

// PhaseView 是单个 phase 的最近一次记录。
type PhaseView struct {
	Name        string    `json:"name"`
	Done        bool      `json:"done"`
	AlreadyDone bool      `json:"alreadyDone,omitempty"`
	AppliedAt   time.Time `json:"appliedAt,omitempty"`
	Error       string    `json:"error,omitempty"`
}

// CertView 是证书汇总。
type CertView struct {
	Total    int      `json:"total"`
	Expiring int      `json:"expiring"`
	Expired  int      `json:"expired"`
	Earliest string   `json:"earliest,omitempty"`
	Names    []string `json:"expiringNames,omitempty"`
}

// UnitView 是 systemd unit 状态。
type UnitView struct {
	Unit           string `json:"unit"`
	Required       bool   `json:"required,omitempty"`
	Active         *bool  `json:"active,omitempty"`
	Enabled        *bool  `json:"enabled,omitempty"`
	Result         string `json:"result,omitempty"`
	ExecMainStatus string `json:"execMainStatus,omitempty"`
	NextRun        string `json:"nextRun,omitempty"`
	Error          string `json:"error,omitempty"`
}

// Report 是 status 的机器可读负载。
type Report struct {
	ConfigName    string      `json:"configName,omitempty"`
	NodeName      string      `json:"nodeName,omitempty"`
	Address       string      `json:"address,omitempty"`
	Roles         []string    `json:"roles,omitempty"`
	Installed     bool        `json:"installed"`
	ClusterID     string      `json:"clusterID,omitempty"`
	InstalledRel  string      `json:"installedRelease,omitempty"`
	RunningRel    string      `json:"runningRelease,omitempty"`
	DesiredFiles  int         `json:"desiredFiles,omitempty"`
	DesiredDrift  []string    `json:"desiredDrift,omitempty"`
	Pending       []string    `json:"pendingRestarts,omitempty"`
	Phases        []PhaseView `json:"phases,omitempty"`
	Certs         *CertView   `json:"certs,omitempty"`
	Units         []UnitView  `json:"units,omitempty"`
	NodeReady     *bool       `json:"nodeReady,omitempty"`
	NodeMessage   string      `json:"nodeMessage,omitempty"`
	DataDir       string      `json:"dataDir"`
	KubemonoVer   string      `json:"kubemonoVersion,omitempty"`
	ClusterConfig string      `json:"clusterConfig,omitempty"`
	// Problems 是 --check 使用的阻塞项；非空时 status --check 返回非 0。
	Problems []string `json:"problems,omitempty"`
}

// New 返回 `kubemono status`。
func New(opts *options.GlobalOptions) *cobra.Command {
	var check bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "只读汇总本机收敛状态（身份/证书/unit/Node）",
		Long:  statusLong,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			rep, err := collect(cmd.Context(), opts)
			if err != nil {
				return err
			}
			rep.Problems = rep.problems()
			if err := utils.Print(cmd.OutOrStdout(), opts.Output, rep, func(w io.Writer) error {
				printTable(w, rep)
				return nil
			}); err != nil {
				return err
			}
			if check && len(rep.Problems) > 0 {
				return fmt.Errorf("status check failed: %s", strings.Join(rep.Problems, "；"))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "健康检查模式：有问题时返回非 0（见 problems 字段）")
	return options.Add(cmd, opts,
		options.ConfigFlag|options.NodeFlag|options.AllowAddressMismatchFlag|options.OutputFlag|
			options.KubeconfigFlag|options.DataDirFlag)
}

func collect(ctx context.Context, opts *options.GlobalOptions) (*Report, error) {
	rep := &Report{DataDir: opts.Paths().Kubemono(), RunningRel: bom.MustLoad().Release}
	cluster, err := opts.LoadCluster()
	if err != nil {
		// status 允许在配置缺失时仍给出本机身份/证书信息。
		rep.NodeMessage = err.Error()
	}
	clusterPath, _ := opts.ClusterConfigPath()
	rep.ClusterConfig = clusterPath
	if cluster != nil {
		if effective, err := opts.EffectiveBOM(cluster); err == nil {
			rep.RunningRel = effective.Release
		}
	}
	paths := opts.Paths()
	if cluster != nil {
		paths = cluster.WithDataDirs(paths)
	}
	var node *clusterconfig.NodeConfig
	if installed, err := (&identity.Store{Dir: opts.Paths().Kubemono()}).Load(); err == nil {
		node = installed
	}
	rep.Installed = node != nil
	if node != nil {
		rep.ClusterID = node.ClusterID
		rep.InstalledRel = node.Release
	}
	if cluster != nil {
		resolver := &identity.Resolver{NodeConfigPath: opts.Paths().NodeConfig()}
		res, err := resolver.Resolve(cluster, opts.Node, opts.AllowAddressMismatch)
		if err != nil {
			return nil, err
		}
		rep.NodeName = res.NodeName()
		rep.Address = res.Address
		if res.Host != nil {
			rep.ConfigName = res.Host.Name
			for _, role := range res.Host.Roles {
				rep.Roles = append(rep.Roles, string(role))
			}
		}
	}

	// desired 快照漂移：只与“上次应用”比较，不重新渲染（避免把集群无关的
	// 当前配置差异误报成漂移；那属于 node repair --diff）。
	if snapshot, err := render.NewStore(paths).Restore(); err == nil && snapshot != nil {
		rep.DesiredFiles = len(snapshot.Files)
		if drift, derr := render.Drift(snapshot); derr == nil {
			for _, item := range drift {
				rep.DesiredDrift = append(rep.DesiredDrift, fmt.Sprintf("%s(%s)", item.File.Path, item.State))
			}
		}
	}

	// phase 记录。
	if records, err := phase.NewFileStore(paths).Load("install"); err == nil {
		names := make([]string, 0, len(records))
		for name := range records {
			names = append(names, name)
		}
		rank := map[string]int{}
		for i, n := range phase.CanonicalOrder {
			rank[n] = i
		}
		sort.Slice(names, func(i, j int) bool {
			ri, iok := rank[names[i]]
			rj, jok := rank[names[j]]
			if iok && jok {
				return ri < rj
			}
			if iok != jok {
				return iok
			}
			return names[i] < names[j]
		})
		for _, name := range names {
			rec := records[name]
			rep.Phases = append(rep.Phases, PhaseView{
				Name: rec.Name, Done: rec.Done, AlreadyDone: rec.AlreadyDone,
				AppliedAt: rec.AppliedAt, Error: rec.Error,
			})
		}
	}

	// 待重启标记。
	if entries, err := filepath.Glob(filepath.Join(paths.State(), "restart-*.json")); err == nil {
		for _, entry := range entries {
			data, err := os.ReadFile(entry)
			if err != nil {
				continue
			}
			var marker phase.RestartMarker
			if json.Unmarshal(data, &marker) == nil {
				rep.Pending = append(rep.Pending, fmt.Sprintf("%s:%s", marker.Phase, strings.Join(marker.Components, ",")))
			}
		}
		sort.Strings(rep.Pending)
	}

	// 证书。
	if statuses, err := certrenew.CheckExpiration(ctx, certrenew.Options{
		Paths: paths, Cluster: cluster, Now: time.Now,
	}); err == nil {
		view := &CertView{Total: len(statuses)}
		var earliest time.Time
		for _, st := range statuses {
			if st.Expired {
				view.Expired++
			}
			if st.Warn || st.Expired {
				view.Expiring++
				view.Names = append(view.Names, st.Name)
			}
			if !st.NotAfter.IsZero() && (earliest.IsZero() || st.NotAfter.Before(earliest)) {
				earliest = st.NotAfter
			}
		}
		if !earliest.IsZero() {
			view.Earliest = earliest.UTC().Format(time.RFC3339)
		}
		sort.Strings(view.Names)
		rep.Certs = view
	}

	// systemd unit（只在 Linux 上查询；其它平台保留错误信息）。
	if runtime.GOOS == "linux" {
		mgr := systemd.Manager{Timeout: 5 * time.Second}
		required := requiredUnits(cluster, rep.Roles)
		units := []string{"kubelet.service", "containerd.service",
			"kube-apiserver.service", "kube-controller-manager.service", "kube-scheduler.service", "etcd.service",
			"kubemono-cert-renew.timer", "kubemono-cert-renew.service"}
		for _, unit := range units {
			view := UnitView{Unit: unit, Required: required[unit]}
			active, err := mgr.IsActive(ctx, unit)
			if err != nil {
				view.Error = err.Error()
			} else {
				view.Active = &active
			}
			enabled, err := mgr.IsEnabled(ctx, unit)
			if err == nil {
				view.Enabled = &enabled
			}
			if show, err := mgr.Show(ctx, unit, "Result", "ExecMainStatus", "NextElapseUSecRealtime"); err == nil {
				view.Result = show["Result"]
				view.ExecMainStatus = show["ExecMainStatus"]
				if raw := show["NextElapseUSecRealtime"]; raw != "" && raw != "n/a" {
					if usec, err := strconv.ParseInt(raw, 10, 64); err == nil && usec > 0 {
						view.NextRun = time.UnixMicro(usec).UTC().Format(time.RFC3339)
					}
				}
			}
			rep.Units = append(rep.Units, view)
		}
	}

	// Node Ready（可选；没有 kubeconfig 时跳过）。
	if client, err := utils.NewClusterClient(opts); err == nil && rep.NodeName != "" {
		nodeObj, err := client.CoreV1().Nodes().Get(ctx, rep.NodeName, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			ready := false
			rep.NodeReady = &ready
			rep.NodeMessage = "Node 尚未注册"
		case err != nil:
			rep.NodeMessage = err.Error()
		default:
			ready := nodeReady(nodeObj)
			rep.NodeReady = &ready
			if !ready {
				rep.NodeMessage = nodeConditionMessage(nodeObj)
			}
		}
	}
	return rep, nil
}

// requiredUnits 返回本机角色/运行时下必须 active 的 unit。
// 纯控制面 master 不要求 kubelet/containerd；custom CRI 不要求 containerd。
func requiredUnits(cluster *clusterconfig.Cluster, roles []string) map[string]bool {
	required := map[string]bool{}
	has := func(role string) bool {
		for _, r := range roles {
			if r == role {
				return true
			}
		}
		return false
	}
	if has("master") {
		required["kube-apiserver.service"] = true
		required["kube-controller-manager.service"] = true
		required["kube-scheduler.service"] = true
		if cluster == nil || !cluster.ControlPlane.Etcd.IsExternal() {
			required["etcd.service"] = true
		}
	}
	if has("worker") {
		required["kubelet.service"] = true
		if cluster == nil || !cluster.Node.Runtime.IsCustom() {
			required["containerd.service"] = true
		}
	}
	return required
}

// problems 返回 --check 的阻塞项。expiring 只作为警告（timer/人工续期会处理），
// expired 才阻塞；unit 只在角色要求时检查。
func (r *Report) problems() []string {
	var out []string
	if !r.Installed {
		out = append(out, "本机未安装（缺少 node.yaml）")
	}
	if len(r.DesiredDrift) > 0 {
		out = append(out, fmt.Sprintf("desired 快照有 %d 处漂移", len(r.DesiredDrift)))
	}
	if len(r.Pending) > 0 {
		out = append(out, fmt.Sprintf("有未完成的重启：%s", strings.Join(r.Pending, "，")))
	}
	if r.Certs != nil && r.Certs.Expired > 0 {
		out = append(out, fmt.Sprintf("有 %d 张证书已过期", r.Certs.Expired))
	}
	if r.NodeReady != nil && !*r.NodeReady {
		out = append(out, "Kubernetes Node 未 Ready")
	}
	for _, u := range r.Units {
		if u.Required && (u.Active == nil || !*u.Active) {
			out = append(out, u.Unit+" 未运行")
		}
		if u.Required && (u.Enabled == nil || !*u.Enabled) {
			out = append(out, u.Unit+" 未设置开机启用")
		}
		if u.Unit == "kubemono-cert-renew.service" && u.Result == "failed" {
			out = append(out, "证书续期 timer 上次执行失败（Result=failed）")
		}
	}
	return out
}

func nodeReady(node *corev1.Node) bool {
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

func nodeConditionMessage(node *corev1.Node) string {
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return fmt.Sprintf("%s: %s", c.Reason, c.Message)
		}
	}
	return "Node Ready 条件缺失"
}

func printTable(w io.Writer, rep *Report) {
	pairs := map[string]string{
		"configName":       rep.ConfigName,
		"nodeName":         rep.NodeName,
		"address":          rep.Address,
		"roles":            strings.Join(rep.Roles, ","),
		"installed":        term.YesNo(rep.Installed),
		"clusterID":        rep.ClusterID,
		"installedRelease": rep.InstalledRel,
		"runningRelease":   rep.RunningRel,
		"dataDir":          rep.DataDir,
	}
	_, _ = fmt.Fprint(w, term.KeyValuesAligned(pairs))
	if rep.Certs != nil {
		fmt.Fprintf(w, "\n证书：%d 张，%d 张进入窗口/已过期（最早到期 %s）\n",
			rep.Certs.Total, rep.Certs.Expiring, valueOrDash(rep.Certs.Earliest))
		for _, name := range rep.Certs.Names {
			fmt.Fprintf(w, "  - %s\n", name)
		}
	}
	if rep.DesiredFiles > 0 || len(rep.DesiredDrift) > 0 {
		fmt.Fprintf(w, "\ndesired 快照：%d 个文件，%d 处漂移\n", rep.DesiredFiles, len(rep.DesiredDrift))
		for _, item := range rep.DesiredDrift {
			fmt.Fprintf(w, "  - %s\n", item)
		}
	}
	if len(rep.Pending) > 0 {
		fmt.Fprintf(w, "\n待重启：%s\n", strings.Join(rep.Pending, "，"))
	}
	if len(rep.Problems) > 0 {
		fmt.Fprintln(w, "\n问题（--check 会返回非 0）：")
		for _, p := range rep.Problems {
			fmt.Fprintf(w, "  - %s\n", p)
		}
	}
	if len(rep.Phases) > 0 {
		fmt.Fprintln(w, "\nphase 最近结果：")
		for _, p := range rep.Phases {
			status := "未完成"
			if p.Done {
				status = "完成"
			}
			if p.AlreadyDone {
				status += "（已跳过）"
			}
			if p.Error != "" {
				status = "失败: " + p.Error
			}
			fmt.Fprintf(w, "  %-20s %s\n", p.Name, status)
		}
	}
	if len(rep.Units) > 0 {
		fmt.Fprintln(w, "\nunit：")
		for _, u := range rep.Units {
			req := ""
			if u.Required {
				req = " required"
			}
			extra := ""
			if u.Result != "" {
				extra += " result=" + u.Result
			}
			if u.ExecMainStatus != "" && u.ExecMainStatus != "0" {
				extra += " exit=" + u.ExecMainStatus
			}
			if u.NextRun != "" {
				extra += " next=" + u.NextRun
			}
			fmt.Fprintf(w, "  %-32s active=%s enabled=%s%s%s %s\n", u.Unit,
				boolValue(u.Active), boolValue(u.Enabled), req, extra, u.Error)
		}
	}
	if rep.NodeReady != nil {
		fmt.Fprintf(w, "\nNode Ready：%s %s\n", term.YesNo(*rep.NodeReady), rep.NodeMessage)
	} else if rep.NodeMessage != "" {
		fmt.Fprintf(w, "\nNode：%s\n", rep.NodeMessage)
	}
}

func valueOrDash(v string) string {
	if v == "" {
		return "-"
	}
	return v
}

func boolValue(v *bool) string {
	if v == nil {
		return "-"
	}
	if *v {
		return "yes"
	}
	return "no"
}
