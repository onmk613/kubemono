package reset

import "kubemono/internal/constants"

// 运行时与组件的固定路径。
const (
	// RuntimeUnitName 是 kubemono 托管 containerd 的 unit 名。
	RuntimeUnitName = "containerd.service"
	// SystemdUnitDir 是 systemd unit 文件目录。
	SystemdUnitDir = "/etc/systemd/system"
)

// ControlPlaneUnits 是第 2 步需要停止并禁用的控制面 unit。
// kubelet 不在这里： 第 3 步单独处理它（先停 kubelet，再停 Pod）。
var ControlPlaneUnits = []string{
	constants.ComponentEtcd + ".service",
	constants.ComponentAPIServer + ".service",
	constants.ComponentControllerManager + ".service",
	constants.ComponentScheduler + ".service",
}

// ComponentUnits 是全部 kubemono 托管的 systemd unit（第 4 步删除 unit 文件）。
var ComponentUnits = append(append([]string{}, ControlPlaneUnits...),
	constants.ComponentKubelet+".service",
)

// unitFilePath 返回 unit 在本机（含测试根）的路径。
func unitFilePath(paths constants.Paths, unit string) string {
	return paths.Resolve(SystemdUnitDir + "/" + unit)
}
