// Package addon 实现 `addon` phase：部署并维护 coredns。
//
// 这里只有一条 apply 路径：把 addon 渲染出的 `render.Object` 交给
// `internal/clusterapply`。渲染（含"必须保留的集群现状"合并）在 internal/addons，
// 写入在本包 → clusterapply，没有第二套 Applier。
package addon

import (
	"context"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"kubemono/internal/addons"
	"kubemono/internal/clusterapply"
	"kubemono/internal/constants"
	"kubemono/internal/phase"
	"kubemono/internal/render"
)

// PhaseName 是 `addon` phase 的名称。
const PhaseName = "addon"

func init() { phase.MustRegister(&Phase{}) }

// component 是一个待应用的附加组件：名字、期望副本数与期望对象。
type component struct {
	name     string
	replicas int32
	objects  *render.ObjectSet
}

// Phase 应用随附的附加组件。
type Phase struct {
	// Client 是集群凭据；install 在控制面上线后注入。
	Client kubernetes.Interface
	// components 覆盖渲染出的组件（仅测试用）。
	components []component
}

// Name 实现 phase.Phase。
func (p *Phase) Name() string { return PhaseName }

// Applies 报告本节点是否应该管理集群附加组件。
// 附加组件是集群级资源：只有 master 参与；重复执行 addon phase 会幂等刷新。
func (p *Phase) Applies(c *phase.Context) bool {
	return c.IsMaster()
}

// client 返回注入的客户端，未注入时回退到 phase context 的客户端。
func (p *Phase) client(c *phase.Context) (kubernetes.Interface, error) {
	if p.Client != nil {
		return p.Client, nil
	}
	client, err := c.KubeClient()
	if err != nil {
		return nil, fmt.Errorf("addon: %w", err)
	}
	return client, nil
}

// enabledComponents 返回需要管理的附加组件（已渲染）。
func (p *Phase) enabledComponents(ctx context.Context, c *phase.Context, client kubernetes.Interface) ([]component, error) {
	if p.components != nil {
		return p.components, nil
	}
	if c.Cluster == nil || c.BOM == nil {
		return nil, fmt.Errorf("addon: 缺少集群配置或 BOM")
	}
	images := map[string]string{}
	for _, name := range []string{constants.ComponentCoreDNS} {
		img, err := c.BOM.Image(name)
		if err != nil {
			return nil, err
		}
		images[name] = img.Reference()
	}
	list, err := addons.All(c.Cluster, images)
	if err != nil {
		return nil, err
	}
	return p.componentsFor(ctx, client, list)
}

// disabledComponents 返回配置中关闭的 addon（只为删除遗留对象）。
func (p *Phase) disabledComponents(ctx context.Context, c *phase.Context, client kubernetes.Interface) ([]component, error) {
	if p.components != nil || c == nil || c.Cluster == nil {
		return nil, nil
	}
	return p.componentsFor(ctx, client, addons.Disabled(c.Cluster))
}

// componentsFor 渲染一组 CoreDNS 配置。
func (p *Phase) componentsFor(ctx context.Context, client kubernetes.Interface, list []addons.CoreDNS) ([]component, error) {
	out := make([]component, 0, len(list))
	for _, a := range list {
		set, err := a.Objects(ctx, client)
		if err != nil {
			return nil, fmt.Errorf("渲染 %s 失败: %w", a.Name(), err)
		}
		out = append(out, component{name: a.Name(), replicas: a.DesiredReplicas(), objects: set})
	}
	return out, nil
}

// Check 报告每个已启用附加组件是否已存在、内容一致且可用；同时检查关闭的
// addon 是否还有遗留对象需要删除。
func (p *Phase) Check(ctx context.Context, c *phase.Context) (bool, error) {
	client, err := p.client(c)
	if err != nil {
		return false, err
	}
	disabled, err := p.disabledComponents(ctx, c, client)
	if err != nil {
		return false, err
	}
	// CoreDNS 关闭后同样要删除旧对象，否则"关闭"只是不 apply，
	// 旧 Deployment 会继续运行。
	for _, comp := range disabled {
		exists, err := objectsAnyExist(ctx, client, comp)
		if err != nil {
			return false, err
		}
		if exists {
			return false, nil
		}
	}
	list, err := p.enabledComponents(ctx, c, client)
	if err != nil {
		return false, err
	}
	for _, comp := range list {
		ready, err := deployed(ctx, client, comp)
		if err != nil {
			return false, err
		}
		if !ready {
			return false, nil
		}
		// 只看"Deployment ready"发现不了 ConfigMap/RBAC/Service 的内容漂移；
		// phase runner 在 Check=true 时直接跳过 Run，这些变更就永远写不进去。
		for _, obj := range comp.objects.Objects() {
			matched, err := clusterapply.Matches(ctx, client, obj)
			if err != nil {
				return false, err
			}
			if !matched {
				return false, nil
			}
		}
	}
	return true, nil
}

// Run 应用每个已启用的附加组件，并删除已关闭 addon 的遗留对象。
func (p *Phase) Run(ctx context.Context, c *phase.Context) error {
	client, err := p.client(c)
	if err != nil {
		return err
	}
	list, err := p.enabledComponents(ctx, c, client)
	if err != nil {
		return err
	}
	disabled, err := p.disabledComponents(ctx, c, client)
	if err != nil {
		return err
	}
	if err := p.removeDisabled(ctx, client, c, disabled); err != nil {
		return err
	}
	if len(list) == 0 {
		c.Reporter().Debugf("没有启用任何附加组件")
		return nil
	}
	if c.DryRun {
		for _, comp := range list {
			c.Reporter().Infof("将应用附加组件 %s（%d 个对象）", comp.name, comp.objects.Len())
		}
		return nil
	}
	for _, comp := range list {
		for _, obj := range comp.objects.Objects() {
			if _, err := clusterapply.Apply(ctx, client, obj); err != nil {
				return fmt.Errorf("应用 %s 的 %s 失败: %w", comp.name, obj.Ref(), err)
			}
		}
	}
	c.Reporter().Result(true, "已应用 %d 个附加组件", len(list))
	return nil
}

// objectsAnyExist 报告某个组件的任一对象是否还在集群里。
func objectsAnyExist(ctx context.Context, client kubernetes.Interface, comp component) (bool, error) {
	for _, obj := range comp.objects.Objects() {
		err := clusterapply.Exists(ctx, client, obj)
		switch {
		case err == nil:
			return true, nil
		case apierrors.IsNotFound(err):
			continue
		default:
			return false, err
		}
	}
	return false, nil
}

// removeDisabled 删除已关闭 addon 的遗留对象。
// 这些对象属于 kubemono 管理，关闭后必须真正移除，否则 Deployment 会继续运行。
func (p *Phase) removeDisabled(ctx context.Context, client kubernetes.Interface, c *phase.Context, disabled []component) error {
	var errs []error
	for _, comp := range disabled {
		if c.DryRun {
			c.Reporter().Infof("将删除已关闭的附加组件 %s（%d 个对象，dry-run）", comp.name, comp.objects.Len())
			continue
		}
		failed := false
		for _, obj := range comp.objects.Objects() {
			if err := clusterapply.Delete(ctx, client, obj); err != nil {
				// 继续删其它对象，最后把错误聚合返回：本次不能报成功，
				// 否则脚本会以为“关闭”已经生效。
				errs = append(errs, fmt.Errorf("删除已关闭附加组件 %s 的 %s 失败：%w", comp.name, obj.Ref(), err))
				failed = true
				continue
			}
		}
		if !failed {
			c.Reporter().Infof("已删除关闭的附加组件 %s", comp.name)
		}
	}
	return errors.Join(errs...)
}

// deployed 报告组件的 Deployment 是否存在、副本数吻合且可用。
func deployed(ctx context.Context, client kubernetes.Interface, comp component) (bool, error) {
	desired := comp.replicas
	if desired <= 0 {
		desired = 1
	}
	dep, err := client.AppsV1().Deployments(constants.KubeSystemNamespace).Get(ctx, comp.name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	// 必须同时比较 spec 与 status：只看 AvailableReplicas >= desired 会把
	// "2 个副本缩到 1 个"误判为已完成（真机实测：CoreDNS 永远缩不下去）。
	if dep.Spec.Replicas == nil || *dep.Spec.Replicas != desired {
		return false, nil
	}
	if dep.Status.ObservedGeneration < dep.Generation {
		return false, nil
	}
	// AvailableReplicas 而不是 ReadyReplicas：前者才是"能服务"的判据。
	return dep.Status.UpdatedReplicas >= desired && dep.Status.AvailableReplicas >= desired, nil
}
