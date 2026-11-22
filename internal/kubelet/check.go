package kubelet

import (
	"context"
	"fmt"
	"reflect"

	"kubemono/internal/constants"
	"kubemono/internal/render"
	"kubemono/internal/systemd"
)

// CheckOptions 是 kubelet 幂等检查的输入。
// 判据：
// 1. 渲染出来的 kubelet 文件都在、内容与权限与期望一致；
// 2. /usr/local/bin 里的二进制与物料一致；
// 3. kubelet.service 处于 active；
// 4. Node 对象 Ready（worker 还需已生成 kubelet.conf）。
type CheckOptions struct {
	// Paths 解析所有约定路径。
	Paths constants.Paths
	// Spec 是渲染出的期望状态。
	Spec *render.Spec
	// Node 是要查询的节点名。
	Node string
	// Installer 校验已安装的二进制；nil 表示跳过该项检查。
	Installer *BinaryInstaller
	// ServiceManager 校验 unit 状态；spec 包含 unit 时必填。
	ServiceManager systemd.ServiceManager
	// NodeClient 校验 Node Ready；nil 视为“无法校验”，此时检查报告“未完成”
	// （Run 会给出真正的错误）。
	NodeClient NodeClient
	// Report 接收诊断消息。
	Report func(string, ...any)
}

func (o CheckOptions) report(format string, args ...any) {
	if o.Report != nil {
		o.Report(format, args...)
	}
}

// Check 报告本节点的 kubelet 是否已完整安装、正在运行并已注册。
func Check(ctx context.Context, opts CheckOptions) (bool, error) {
	if len(DesiredFiles(opts.Spec)) == 0 {
		return false, fmt.Errorf("渲染结果里没有 kubelet 的文件（component=%s），无法判断 kubelet 是否就绪", constants.ComponentKubelet)
	}
	drifted, err := DesiredFilesDrifted(opts.Spec)
	if err != nil {
		return false, err
	}
	if len(drifted) > 0 {
		for _, d := range drifted {
			opts.report("kubelet 文件需要修复：%s（%s）", d.File.Path, d.State)
		}
		return false, nil
	}
	if opts.Installer != nil {
		ok, err := opts.Installer.Verify(ctx, constants.ComponentKubelet)
		if err != nil {
			return false, err
		}
		if !ok {
			opts.report("kubelet 二进制缺失或与期望版本不一致，需要重新安装")
			return false, nil
		}
	}
	if _, hasUnit := UnitFile(opts.Spec, opts.Paths); hasUnit {
		if opts.ServiceManager == nil {
			return false, fmt.Errorf("渲染结果包含 %s，但没有配置 ServiceManager，无法确认服务状态", UnitName)
		}
		active, err := opts.ServiceManager.IsActive(ctx, UnitName)
		if err != nil {
			return false, err
		}
		if !active {
			opts.report("%s 未运行，需要启动", UnitName)
			return false, nil
		}
		if ec, ok := opts.ServiceManager.(systemd.EnabledChecker); ok {
			enabled, err := ec.IsEnabled(ctx, UnitName)
			if err != nil {
				return false, err
			}
			if !enabled {
				opts.report("%s 未设置开机自启，需要 enable", UnitName)
				return false, nil
			}
		}
	}
	if opts.Node == "" {
		return false, fmt.Errorf("节点名为空，无法确认 kubelet 是否已注册为 Node")
	}
	if nilInterface(opts.NodeClient) {
		opts.report("没有 Kubernetes 客户端，无法确认节点 %s 是否 Ready", opts.Node)
		return false, nil
	}
	ready, err := opts.NodeClient.NodeReady(ctx, opts.Node)
	if err != nil {
		// 临时性 API 错误不应让整个 install 失败：按“未完成”处理，Run 会重试等待。
		opts.report("查询节点 %s 的 Ready 状态失败：%v", opts.Node, err)
		return false, nil
	}
	if !ready {
		opts.report("节点 %s 尚未 Ready（或 kubelet 版本与 BOM 不一致）", opts.Node)
		return false, nil
	}
	return true, nil
}

// DesiredFilesDrifted 返回与渲染期望状态不一致（缺失、被修改或权限错误）的
// kubelet 文件。
func DesiredFilesDrifted(spec *render.Spec) ([]render.DriftItem, error) {
	files := DesiredFiles(spec)
	if len(files) == 0 {
		return nil, nil
	}
	return render.Drift(&render.Spec{Node: spec.Node, Files: files})
}

// nilInterface 报告接口值是否为 nil，包括接口中存放的类型化 nil 指针。
func nilInterface(v any) bool {
	if v == nil {
		return true
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Slice, reflect.Map, reflect.Func, reflect.Chan:
		return rv.IsNil()
	default:
		return false
	}
}
