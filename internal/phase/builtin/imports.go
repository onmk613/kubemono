// 单一注册点：所有构建 internal/cli 的二进制都通过
// 这里获得同一套 phase，避免每个 main 各自空导入、漏掉某个包。
package builtin

import (
	_ "kubemono/internal/phase/addon"
	_ "kubemono/internal/phase/certs"
	_ "kubemono/internal/phase/clusterinit"
	_ "kubemono/internal/phase/controlplane"
	_ "kubemono/internal/phase/etcd"
	_ "kubemono/internal/phase/etcdhealth"
	_ "kubemono/internal/phase/hostinit"
	_ "kubemono/internal/phase/kubeconfig"
	_ "kubemono/internal/phase/kubelet"
	_ "kubemono/internal/phase/localproxy"
	_ "kubemono/internal/phase/markcontrolplane"
	_ "kubemono/internal/phase/network"
	_ "kubemono/internal/phase/preflight"
	_ "kubemono/internal/phase/runtime"
)
