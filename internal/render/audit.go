package render

// auditPolicyYAML 是 apiserver 审计策略的默认内容；cluster.yaml 的
// controlPlane.audit.policy 可以覆盖它，空值保持这份安全默认。
// 策略的两条硬要求：
// 1. 修改或删除准入策略属于审计重点事件，单独记录；
// 2. 其余请求只记录元数据：审计日志不应该成为集群数据的一个副本。
// kubemono 的集群级对象（cluster-info、kubemono-cluster、kubemono-certs）都在
// kube-system/kube-public，凭据类读取由下面的 Metadata 规则统一覆盖。
const auditPolicyYAML = `apiVersion: audit.k8s.io/v1
kind: Policy
# 已知的、不需要审计的只读请求。
omitStages:
  - RequestReceived
rules:
  # ① 准入策略的修改属于重点事件。
  - level: Request
    verbs: [create, update, patch, delete]
    resources:
      - group: admissionregistration.k8s.io
        resources: ["validatingadmissionpolicies", "validatingadmissionpolicybindings", "validatingwebhookconfigurations", "mutatingwebhookconfigurations"]
  # ② 集群配置与 RBAC 的修改。
  - level: Request
    verbs: [create, update, patch, delete]
    resources:
      - group: rbac.authorization.k8s.io
        resources: ["clusterroles", "clusterrolebindings", "roles", "rolebindings"]
  # ③ 凭据类资源的读取：只记录元数据（谁读了哪个对象）。
  - level: Metadata
    resources:
      - group: ""
        resources: [secrets, serviceaccounts, "serviceaccounts/token"]
      - group: certificates.k8s.io
        resources: ["certificatesigningrequests"]
  # ④ 其余请求不记录 body，避免审计日志变成数据副本。
  - level: Metadata
`
