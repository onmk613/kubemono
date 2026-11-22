// Package markcontrolplane 实现 `mark-control-plane` phase：把配置里的
// `hosts[].labels` 与 `hosts[].taints` 写到 Node 对象上。
// 只做“设置/更新”：从配置里删掉一个标签或污点不会主动从 Node 上移除，
// 避免误删 kubelet、云厂商或 CNI 写入的内容。
// taints 在 kubelet 注册时也会通过 registerWithTaints 带入，本 phase 负责
// 后续的热更新。
// 凭据：master 用 admin.conf；worker 用自己的 kubelet.conf（system:node:<name>）。
// NodeRestriction 禁止 system:node 修改自身 taints，因此 worker 的污点变更需要
// 显式提供 admin 凭据。
// 时序：install 里本 phase 紧跟控制面之后执行，此时 kubelet 往往还没完成注册，
// 因此 Run 会在有限时间内等待 Node 对象出现，而不是立刻失败。
package markcontrolplane

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	clusterconfig "kubemono/internal/config"
	"kubemono/internal/phase"
)

// PhaseName 是 `mark-control-plane` phase 的名称。
const PhaseName = "mark-control-plane"

// LabelControlPlane 是标准的控制面标签。
const LabelControlPlane = "node-role.kubernetes.io/control-plane"

// Node 注册等待的默认值：kubelet 在 apiserver 可用后通常几秒内完成注册。
const (
	DefaultRegisterTimeout  = 3 * time.Minute
	DefaultRegisterInterval = 2 * time.Second
)

func init() { phase.MustRegister(&Phase{}) }

// Phase 给 Node 对象打标签、设置污点。
type Phase struct {
	// Client 是 Kubernetes 客户端；install 在控制面就绪后注入。
	Client kubernetes.Interface
	// WaitKnobs 限定等待 Node 注册的行为（可注入，便于测试；默认值见
	// DefaultRegisterTimeout / DefaultRegisterInterval）。
	phase.WaitKnobs
}

// Name 实现 phase.Phase。
func (p *Phase) Name() string { return PhaseName }

// Applies 报告本节点是否需要打标签。
// 所有注册为 Node 的主机（纯 worker 与 [master, worker]）都需要；
// 纯控制面 master 不注册 Node，没有可打的标签。
func (p *Phase) Applies(c *phase.Context) bool {
	host, ok := c.Self()
	return ok && host.IsWorker()
}

// client 返回注入的客户端，未注入时回退到 phase 上下文里的。
func (p *Phase) client(c *phase.Context) (kubernetes.Interface, error) {
	if p.Client != nil {
		return p.Client, nil
	}
	client, err := c.KubeClient()
	if err != nil {
		return nil, fmt.Errorf("mark-control-plane: %w（节点注册后才能打标签）", err)
	}
	return client, nil
}

// desiredLabels 计算本节点必须携带的标签。
func desiredLabels(c *phase.Context) (map[string]string, error) {
	host, ok := c.Self()
	if !ok {
		return nil, fmt.Errorf("mark-control-plane: 无法确定本机在集群配置中的条目")
	}
	out := map[string]string{}
	if host.IsMaster() {
		// 空值标签是 k8s 惯例（node-role.kubernetes.io/<role>=""）。
		out[LabelControlPlane] = ""
	}
	for k, v := range host.Labels {
		if k == "" {
			continue
		}
		out[k] = v
	}
	return out, nil
}

// desiredTaints 解析 hosts[].taints 为 Node API 的 Taint。
// 格式与 kubelet register-with-taints 一致：key[=value][:Effect]。
func desiredTaints(c *phase.Context) ([]corev1.Taint, error) {
	host, ok := c.Self()
	if !ok {
		return nil, fmt.Errorf("mark-control-plane: 无法确定本机在集群配置中的条目")
	}
	out := make([]corev1.Taint, 0, len(host.Taints))
	seen := map[string]bool{}
	for _, raw := range host.Taints {
		taint, err := parseTaint(raw)
		if err != nil {
			return nil, fmt.Errorf("hosts[].taints 条目 %q: %w", raw, err)
		}
		id := taintIdentity(taint)
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, taint)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Key != out[j].Key {
			return out[i].Key < out[j].Key
		}
		return out[i].Effect < out[j].Effect
	})
	return out, nil
}

// parseTaint 解析并校验 taint 声明（统一走 clusterconfig.ParseTaint）。
// 写进 Node 对象的 taint 必须严格合法：kubelet 会拒绝非法 effect。
func parseTaint(raw string) (corev1.Taint, error) {
	key, value, effect, err := clusterconfig.ParseTaint(raw)
	if err != nil {
		return corev1.Taint{}, err
	}
	return corev1.Taint{Key: key, Value: value, Effect: corev1.TaintEffect(effect)}, nil
}

func taintIdentity(t corev1.Taint) string { return t.Key + "\x00" + string(t.Effect) }

func taintSpec(t corev1.Taint) string {
	if t.Value == "" {
		return t.Key + ":" + string(t.Effect)
	}
	return t.Key + "=" + t.Value + ":" + string(t.Effect)
}

func hasTaint(list []corev1.Taint, want corev1.Taint) bool {
	for _, t := range list {
		if t.Key == want.Key && t.Value == want.Value && t.Effect == want.Effect {
			return true
		}
	}
	return false
}

// Check 报告 Node 是否已经带有全部期望标签/污点。
func (p *Phase) Check(ctx context.Context, c *phase.Context) (bool, error) {
	client, err := p.client(c)
	if err != nil {
		return false, err
	}
	wantLabels, err := desiredLabels(c)
	if err != nil {
		return false, err
	}
	wantTaints, err := desiredTaints(c)
	if err != nil {
		return false, err
	}
	node, err := client.CoreV1().Nodes().Get(ctx, c.NodeName(), metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			// 节点还没注册：kubelet 尚未完成 TLS Bootstrap，
			// 这不是错误，只是"还没到这一步"。
			return false, nil
		}
		return false, err
	}
	for k, v := range wantLabels {
		if got, ok := node.Labels[k]; !ok || got != v {
			return false, nil
		}
	}
	for _, want := range wantTaints {
		if !hasTaint(node.Spec.Taints, want) {
			return false, nil
		}
	}
	return true, nil
}

// Run 设置期望的标签与污点（不删除 Node 上已有的其他项）。
func (p *Phase) Run(ctx context.Context, c *phase.Context) error {
	client, err := p.client(c)
	if err != nil {
		return err
	}
	wantLabels, err := desiredLabels(c)
	if err != nil {
		return err
	}
	wantTaints, err := desiredTaints(c)
	if err != nil {
		return err
	}
	if c.DryRun {
		node, err := client.CoreV1().Nodes().Get(ctx, c.NodeName(), metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			c.Reporter().Infof("将等待节点 %s 注册后设置 %d 个标签、%d 个污点（dry-run）",
				c.NodeName(), len(wantLabels), len(wantTaints))
			return nil
		}
		if err != nil {
			return err
		}
		for k, v := range wantLabels {
			if got, ok := node.Labels[k]; !ok || got != v {
				c.Reporter().Infof("将设置节点 %s 的标签 %s=%q（dry-run）", c.NodeName(), k, v)
			}
		}
		for _, want := range wantTaints {
			if !hasTaint(node.Spec.Taints, want) {
				c.Reporter().Infof("将设置节点 %s 的污点 %s（dry-run）", c.NodeName(), taintSpec(want))
			}
		}
		return nil
	}
	if _, err := p.waitRegistered(ctx, c, client); err != nil {
		return err
	}
	// kubelet 在注册/心跳时会并发更新同一个 Node 对象；直接 Update 很容易
	// 撞上 "object has been modified"。必须 RetryOnConflict 并每次重新 Get。
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		return mergeNode(ctx, client, c.NodeName(), wantLabels, wantTaints)
	})
	if err != nil {
		if apierrors.IsForbidden(err) && strings.Contains(err.Error(), "not allowed to modify taints") {
			// system:node 身份受 NodeRestriction 限制，不能修改自身 taints。
			return fmt.Errorf("污点变更需要 admin 凭据（NodeRestriction 禁止 system:node 修改自身 taints）："+
				"请用 --kubeconfig /etc/kubernetes/admin.conf 重新执行，或重新加入节点: %w", err)
		}
		return fmt.Errorf("更新节点 %s 的标签/污点: %w", c.NodeName(), err)
	}
	c.Reporter().Infof("已收敛节点 %s 的标签与污点", c.NodeName())
	return nil
}

// mergeNode 把期望的标签/污点合进一次 Get 到的 Node 并 Update；
// 没有差异时不发写请求。合并只增改、不删除 Node 上已有的其它键。
func mergeNode(ctx context.Context, client kubernetes.Interface, name string, wantLabels map[string]string, wantTaints []corev1.Taint) error {
	node, err := client.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	labels := make(map[string]string, len(node.Labels)+len(wantLabels))
	for k, v := range node.Labels {
		labels[k] = v
	}
	changed := false
	for k, v := range wantLabels {
		if got, ok := labels[k]; !ok || got != v {
			labels[k] = v
			changed = true
		}
	}
	taints := append([]corev1.Taint(nil), node.Spec.Taints...)
	for _, want := range wantTaints {
		if hasTaint(taints, want) {
			continue
		}
		replaced := false
		for i := range taints {
			if taintIdentity(taints[i]) == taintIdentity(want) {
				taints[i] = want
				replaced = true
				changed = true
				break
			}
		}
		if !replaced {
			taints = append(taints, want)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	node.Labels = labels
	node.Spec.Taints = taints
	_, err = client.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
	return err
}

// waitRegistered 轮询直到 Node 对象出现。
// 只有 NotFound 与临时性错误会重试；超时后给出可操作的错误（kubelet 日志、
// CSR 审批）。
func (p *Phase) waitRegistered(ctx context.Context, c *phase.Context, client kubernetes.Interface) (*corev1.Node, error) {
	timeout := p.TimeoutOrDefault(DefaultRegisterTimeout)
	interval := p.IntervalOrDefault(DefaultRegisterInterval)
	sleep := p.ResolveSleep()
	name := c.NodeName()
	var waited time.Duration
	logged := false
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		node, err := client.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
		if err == nil {
			return node, nil
		}
		if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
			// 权限问题不会自己好转，立刻失败。
			return nil, fmt.Errorf("读取节点 %s 被拒绝：%w（master 需要 admin.conf，worker 需要自己的 kubelet.conf）", name, err)
		}
		if waited >= timeout {
			return nil, fmt.Errorf("等待节点 %s 注册超时（%s）：最后一次错误 %v。请查看 `journalctl -u kubelet`，worker 还需确认 CSR 已批准（kubectl get csr）", name, timeout, err)
		}
		if !logged {
			c.Reporter().Infof("等待节点 %s 注册（最长 %s）", name, timeout)
			logged = true
		}
		sleep(interval)
		waited += interval
	}
}
