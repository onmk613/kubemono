package config

import (
	"fmt"
	"strings"

	"kubemono/internal/constants"
)

// reservedExtraArgs 列出不允许通过 extraArgs 覆盖的 flag。
//
// 这些 flag 决定“这是哪个集群、信任谁、数据写到哪里”：覆盖它们不会得到
// 一个合法的集群，只会得到组件起不来或安全基线被绕过的节点。extraArgs 的
// 定位是“上游默认值之外的调优”，不是“重新实现 kubemono 的接线”。
//
// 错误信息必须给出替代方案：大多数情况下运维想改的东西有专门字段
// （pki/etcd/network/audit），或者本来就不该改。
var reservedExtraArgs = map[string]map[string]string{
	constants.ComponentAPIServer: {
		"advertise-address":                "由 cluster.hosts[].address 决定",
		"allow-privileged":                 "固定为 true（kubemono 的启动基线）",
		"authorization-mode":               "固定为 Node,RBAC（安全基线）",
		"client-ca-file":                   "由 controlPlane.pki 管理",
		"enable-admission-plugins":         "固定包含 NodeRestriction（安全基线）",
		"enable-bootstrap-token-auth":      "固定为 true（join 依赖它）",
		"etcd-cafile":                      "由 controlPlane.etcd 管理",
		"etcd-certfile":                    "由 controlPlane.etcd 管理",
		"etcd-keyfile":                     "由 controlPlane.etcd 管理",
		"etcd-servers":                     "由 controlPlane.etcd 管理",
		"kubelet-client-certificate":       "由 controlPlane.pki 管理",
		"kubelet-client-key":               "由 controlPlane.pki 管理",
		"proxy-client-cert-file":           "由 controlPlane.pki 管理",
		"proxy-client-key-file":            "由 controlPlane.pki 管理",
		"requestheader-client-ca-file":     "由 controlPlane.pki 管理",
		"secure-port":                      "固定为 6443（端口清单与证书 SAN 依赖它）",
		"service-account-key-file":         "由 controlPlane.pki 管理",
		"service-account-signing-key-file": "由 controlPlane.pki 管理",
		"service-cluster-ip-range":         "由 network.serviceCIDR 决定",
		"tls-cert-file":                    "由 controlPlane.pki 管理",
		"tls-private-key-file":             "由 controlPlane.pki 管理",
	},
	constants.ComponentControllerManager: {
		"allocate-node-cidrs":              "由 network.podCIDR 决定",
		"authentication-kubeconfig":        "由 kubeconfig phase 生成",
		"authorization-kubeconfig":         "由 kubeconfig phase 生成",
		"bind-address":                     "固定为 127.0.0.1",
		"client-ca-file":                   "由 controlPlane.pki 管理",
		"cluster-cidr":                     "由 network.podCIDR 决定",
		"cluster-signing-cert-file":        "由 controlPlane.pki 管理",
		"cluster-signing-key-file":         "由 controlPlane.pki 管理",
		"controllers":                      "固定包含 bootstrapsigner,tokencleaner",
		"kubeconfig":                       "由 kubeconfig phase 生成",
		"node-cidr-mask-size":              "由 network.nodeCIDRMaskSize 决定",
		"requestheader-client-ca-file":     "由 controlPlane.pki 管理",
		"root-ca-file":                     "由 controlPlane.pki 管理",
		"secure-port":                      "固定为 10257",
		"service-account-private-key-file": "由 controlPlane.pki 管理",
		"use-service-account-credentials":  "固定为 true",
	},
	constants.ComponentScheduler: {
		"authentication-kubeconfig": "由 kubeconfig phase 生成",
		"authorization-kubeconfig":  "由 kubeconfig phase 生成",
		"bind-address":              "固定为 127.0.0.1",
		"kubeconfig":                "由 kubeconfig phase 生成",
		"secure-port":               "固定为 10259",
	},
	constants.ComponentKubelet: {
		"bootstrap-kubeconfig":       "由 kubeconfig phase 生成",
		"cert-dir":                   "由 node.kubelet.rootDir 管理",
		"config":                     "由渲染器生成；调优请用 node.kubelet.configPatch",
		"container-runtime-endpoint": "由 node.runtime 决定",
		"hostname-override":          "由 nodeName 策略决定",
		"kubeconfig":                 "由 kubeconfig phase 生成",
		"root-dir":                   "请用 node.kubelet.rootDir",
	},
	constants.ComponentEtcd: {
		"advertise-client-urls":       "由 controlPlane.etcd.local.dataDir / 节点地址决定",
		"cert-file":                   "由 controlPlane.pki 管理",
		"client-cert-auth":            "固定为 true",
		"data-dir":                    "请用 controlPlane.etcd.local.dataDir（路径逻辑依赖它）",
		"initial-advertise-peer-urls": "由节点身份与配置决定",
		"initial-cluster":             "由 etcd phase 的成员发现决定",
		"initial-cluster-state":       "由 init/join 模式决定",
		"key-file":                    "由 controlPlane.pki 管理",
		"listen-client-urls":          "由节点地址决定",
		"listen-peer-urls":            "由节点地址决定",
		"name":                        "由 nodeName 决定",
		"peer-cert-file":              "由 controlPlane.pki 管理",
		"peer-client-cert-auth":       "固定为 true",
		"peer-key-file":               "由 controlPlane.pki 管理",
		"peer-trusted-ca-file":        "由 controlPlane.pki 管理",
		"trusted-ca-file":             "由 controlPlane.pki 管理",
	},
	constants.ComponentKubeRouter: {
		"enable-cni":        "固定为 false（CNI 由 kubemono 的 conflist 管理）",
		"hostname-override": "由 downward API 的 NODE_NAME 提供",
		"kubeconfig":        "由 ServiceAccount token 生成",
		"run-service-proxy": "固定为 false（Service 代理由 kube-proxy 承担）",
	},
}

// ReservedExtraArgReason 返回某个组件 flag 被保留的原因；未保留时返回空串。
// 由 validate 与 profile 共用，避免“校验放过、渲染覆盖”的两套语义。
func ReservedExtraArgReason(component, flag string) string {
	return reservedExtraArgs[component][flag]
}

// ValidateExtraArgName 校验 extraArgs 的 flag 名能否安全写进 ExecStart。
// 与 profile.RenderFlags 共用同一份规则：校验阶段通过的名字，渲染阶段也必须通过。
func ValidateExtraArgName(name string) error {
	if name == "" {
		return fmt.Errorf("flag 名不能为空")
	}
	if strings.HasPrefix(name, "-") {
		return fmt.Errorf("flag 名 %q 不能带前导 \"-\"（写 flagname，不要写 --flagname）", name)
	}
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '.' {
			continue
		}
		return fmt.Errorf("flag 名 %q 含非法字符 %q（只允许字母、数字、- 和 .）", name, r)
	}
	return nil
}
