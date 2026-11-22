package kubelet

import "kubemono/internal/execx"

// NewExecRunner 返回当前平台的生产执行器。
func NewExecRunner() CommandRunner { return execx.Default() }
