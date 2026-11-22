// Package kubelet 负责把 kubelet 真正跑起来。
// 职责边界（“应该是什么”由 internal/render 决定，“怎么变成那样”由本包决定）：
// - 二进制安装：把 release 物料里的 kubelet 二进制直接复制到 /usr/local/bin/kubelet；
// - unit 引用：kubelet 的 systemd unit 由 internal/render 渲染，本包只提供路径
// 与安装/校验所需的文件对照（UnitPath / UnitFile）；
// - 服务控制：通过 systemctl 启停 kubelet（ServiceManager 接口，便于测试）；
// - 就绪判断：等待 TLS Bootstrap 产生 kubelet.conf，并等待 Node Ready
// （NodeClient 接口 + WaitForBootstrap），只告警不审批 serving CSR。
// 所有 Linux 专有行为都通过 systemd/execx 端口或 //go:build linux 的文件实现
// （见 service.go、runner.go），非 linux 平台编译成返回明确错误的桩，
// 因此本包在开发机（darwin）上也能编译、也能跑单元测试。
package kubelet

import (
	"kubemono/internal/constants"
	"kubemono/internal/render"
)

// Kubelet systemd unit 的位置与名字（两种部署模式都一样：kubelet 始终由
// systemd 运行，）。
const (
	// UnitName 是 kubelet systemd unit 的名字。
	UnitName = constants.ComponentKubelet + ".service"
	// UnitDir 是 systemd unit 文件所在目录。
	UnitDir = "/etc/systemd/system"
	// BinaryName 是 kubelet 可执行文件名。
	BinaryName = constants.ComponentKubelet
	// BinaryInstallPath 是所有组件与脚本使用的稳定路径。
	BinaryInstallPath = constants.DefaultLocalBinDir + "/" + BinaryName
)

// UnitPath 返回 kubelet unit 在本机上的路径（遵循 Paths.Root）。
func UnitPath(paths constants.Paths) string {
	return paths.Resolve(UnitDir + "/" + UnitName)
}

// UnitFile 返回渲染结果中 kubelet unit 的内容。
// 第二个返回值为 false 表示这份渲染结果里没有 kubelet unit（只会出现在注入的
// 测试 Spec 里）：internal/render 始终渲染 kubelet.service（，离线
// 安装没有发行版的 kubelet 包可依赖）。
func UnitFile(spec *render.Spec, paths constants.Paths) ([]byte, bool) {
	if spec == nil {
		return nil, false
	}
	f, ok := spec.Find(UnitPath(paths))
	if !ok {
		return nil, false
	}
	return f.Content, true
}

// DesiredFiles 返回 kubelet 组件拥有的渲染文件。
func DesiredFiles(spec *render.Spec) []render.File {
	if spec == nil {
		return nil
	}
	return spec.FilesOf(constants.ComponentKubelet)
}
