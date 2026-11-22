# kubemono

`kubemono` 是一个面向 Linux 的 Kubernetes 发行版安装与生命周期工具：**单二进制、无守护进程、离线优先、人工编排**。它把集群引导、主机基线、PKI、网络、附加组件、离线物料的**造包与消费**全部收进同一个二进制：`kubemono bundle pack` 造包、`bundle load` 导入；kubemono 自身在节点上只有一个二进制，运行组件由它安装到 `/usr/local/bin`，之后由 systemd/kubelet 直接运行。

- 版本：由 `internal/bom/bom.yaml` 的 `release` 决定；当前开发基线为 `v1.37.1-km.1`（尚未正式定版）
- 目标平台：Linux `amd64` / `arm64`（Debian 13 完整验证，其他见支持矩阵）
- 许可：MIT

> 实现与安全约束见 §6 架构、§7 离线物料、§8 安全模型。

---

## 1. 设计原则

| 原则 | 含义 |
|---|---|
| 单二进制 | 节点侧所有 phase 通过空导入注册到同一进程，无插件、无 sidecar；造包（`bundle pack`）与消费（`bundle load`）都在 `kubemono` 里，没有第二个发布物 |
| 无守护进程 | 没有常驻 agent/operator/timer；变更由人或 CI 逐台触发 |
| 幂等 phase | 每一步自判 `Applies/Check/Run`；需要回滚的步骤在自身 `Run` 内逆序撤销，失败可续跑 |
| 离线优先 | 组件版本、镜像与物料清单都进归档；归档摘要 + 可选的逐文件 sha256 |
| 主机即事实 | 渲染是纯函数，落盘用原子写；漂移由 live 状态与 desired 比较发现 |

---

## 2. 系统要求

| 项 | 要求 |
|---|---|
| OS | Debian 13（完整验证）；其他 systemd Linux 跟随上游官方二进制，未做发行版级完整验证 |
| 内核 | ≥ 5.4（以发行版实际内核为准） |
| 资源 | ≥ 2 vCPU、≥ 2 GiB 内存；数据目录满足磁盘/inode 下限 |
| cgroup | 必须 v2（Kubernetes ≥ 1.35 的 kubelet 在 cgroup v1 上不会启动；preflight 直接失败） |
| 容器 | 不支持在容器内执行 host-init |
| 宿主命令 | `systemctl`、`modprobe`、`sysctl`；worker 还需要 `iptables`（bridge/portmap/kube-proxy 依赖）；`kubeProxy.mode=nftables` 时还需要 `nft`。preflight 会检查 |
| 端口 | 6443、2379、2380、10250、20244、10256 等；HA 外部入口需自备 LB |
| 权限 | 安装/重置/证书命令需 root；集群级命令需 master 上的 `admin.conf` 或 `super-admin.conf` |

---

## 3. 快速开始

### 3.1 构建

```bash
make build-linux          # 发布二进制：bin/kubemono-linux-amd64、bin/kubemono-linux-arm64
make dist                 # 物料归档 dist/kubemono-<release>-linux-amd64[-lite].tar.zst + .sha256
make dist ARCHS=amd64,arm64 IMAGES=1   # 双架构 + 镜像
# 仅本机调试：
make build                # bin/kubemono（当前 OS，不可用于 Linux 节点）
```

也可以直接：

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o kubemono ./cmd/kubemono
```

### 3.2 节点角色组合

> 外部 etcd 由用户自行部署；kubemono 只读取 endpoints/证书并做启动前健康检查，
> 外部 etcd 的字段、TLS 与迁移要求见 [`docs/cluster-config.md`](docs/cluster-config.md) 的 `controlPlane.etcd`。

`cluster.hosts[].roles` 可以组合：

| 组合 | 含义 |
|---|---|
| `[master]` | **纯控制面**：只跑 apiserver/kcm/scheduler（本地 etcd 模式还跑 etcd），不装 kubelet/containerd/CNI，不注册 Node；不可调度业务 Pod |
| `[master, worker]` | **混合节点**：控制面 + 业务负载；有 kubelet，默认可调度（不自动加 control-plane 污点） |
| `[worker]` | 纯工作负载 |


因此可以同一集群里一部分 master 是纯控制面、一部分 master 与 worker 混合，worker 独立承载业务。纯控制面 master 不会出现在 `kubectl get nodes`。

Service 代理统一由 **kube-proxy** 承担，kubemono 以 DaemonSet 部署（hostNetwork，
使用自己的 ServiceAccount token 访问 apiserver）：

- `network.plugin=kube-router`（默认）：kube-router 只做 CNI/路由/网络策略，
  固定 `--run-service-proxy=false`，不自动替换 kube-proxy；
- `network.plugin=none`：kubemono 不写 CNI 配置，但默认仍部署 kube-proxy；
  如果第三方 CNI 自带 kube-proxy replacement（Cilium/Calico 等），把
  `network.kubeProxy.enabled` 设为 `false`。

```yaml
network:
  kubeProxy:
    enabled: true        # 默认 true
    mode: ipvs           # ipvs（默认）| iptables | nftables
    scheduler: rr        # 仅 ipvs 模式生效
```

`nftables` 模式要求每个节点内核 **>= 5.13**：`config validate` 会提示，
`preflight` 会按本机内核严格校验并阻止安装。切换到 `iptables`/`nftables` 时
kube-proxy 会自行清理另一种模式的规则。

自定义 CRI（`runtime.custom`）、第三方 CNI（`network.plugin: none`）以及
Service 代理的完整写法见
见 [`docs/cluster-config.md`](docs/cluster-config.md) 的 `node.runtime.custom` / `network.plugin`，
命令行用法见 [`docs/cli-reference.md`](docs/cli-reference.md)。

### 3.3 准备配置

```bash
kubemono config default > cluster.yaml       # 生成带注释的默认配置
kubemono config validate -f cluster.yaml     # 严格校验（未知字段会报错）
```

从 [`examples/cluster.yaml`](examples/cluster.yaml) 复制并按环境修改：`cluster.hosts`、`address`、`roles`、`controlPlane.externalEndpoints`、`network.podCIDR/serviceCIDR` 等。

如果集群已经有稳定 VIP/DNS，把它写进 `controlPlane.externalEndpoints` 即可：worker
直接使用该入口，不部署本地 haproxy；kubelet 使用列表中的第一个 endpoint，因此多
master 下这里必须填 VIP/DNS，不能填某台 master 的地址。

> 故障期需要重跑网络层（例如 kube-router DaemonSet 被误删）或修复安装遗留问题时，
> 在 master 上执行 `node repair`（整表幂等重跑）。

### 3.4 初始化第一台 master

```bash
# 在 m1 上，root 执行
kubemono bundle load /mnt/usb/kubemono/kubemono-<release>-linux-amd64.tar.zst
kubemono init -f cluster.yaml

# 初始化完成后，按角色签发其他节点的加入凭证（不落盘）：
kubemono token create                 # worker（默认）
kubemono token create --role master   # 新增控制面/master 时才需要
```

`init` 只负责把集群建起来：执行 preflight/host-init/runtime/certs/kubeconfig/kubelet/etcd/control-plane/cluster-init/network/addon，写本机身份与集群级资源。**init 不生成任何 join 凭证**。

`token create` 默认签发 worker 凭证：只注册 bootstrap token，并打印一条 worker join 命令；它不会碰 `certificateKey` 和 `kube-system/kubemono-certs`。`--role master` 才签发 master 凭证，同时轮换 certificateKey、刷新加密证书包，并打印一条 master join 命令。每次调用只打印一条命令；进度和告警在 stderr。

master 上还会创建 `~/.kube/config -> /etc/kubernetes/admin.conf` 软链，之后直接执行 `kubectl` 即可，不需要每次带 `--kubeconfig`。如果 `~/.kube/config` 已存在且不是 kubemono 管理的 admin.conf 软链，kubemono 不会覆盖它。

### 3.5 加入节点

```bash
# worker / 新 master 上，root 执行
kubemono bundle load /mnt/usb/kubemono/kubemono-<release>-linux-amd64.tar.zst

# worker：复制 `kubemono token create` 输出的 worker 命令
kubemono join 10.0.0.1 --token abcdef.0123456789abcdef \
  --discovery-token-ca-cert-hash sha256:... -f /usr/local/kubemono/cluster.yaml

# master / etcd：加 --control-plane 与 --certificate-key
kubemono join 10.0.0.1 --token abcdef.0123456789abcdef \
  --discovery-token-ca-cert-hash sha256:... \
  --control-plane --certificate-key <64 位 hex> -f /usr/local/kubemono/cluster.yaml
# runtime phase 会在 containerd 就绪后自动导入离线镜像（pause/haproxy/kube-router/kube-proxy/CoreDNS）
```

- worker 从 `kube-public/cluster-info` 取得并校验 CA，再走 TLS Bootstrap 获取 kubelet 凭据；
  master 额外用 `--certificate-key` 解密证书包。
- `--certificate-key` 只用于 master；纯 worker 传了会被拒绝；master 不传也会被拒绝。
- bootstrap token 有效期 **2h**（短期凭证模型，硬上限也是 2h）；
  命令在有效期内可复用，过期后不能再用。

#### join 命令过期 / 很久以后加节点

bootstrap token 有效期 **2h**；过期后不能再用，但**不需要重建集群**。
在任意一台已初始化、有 `admin.conf` 和集群 PKI 的 master 上按角色重新签发：

```bash
# 只加 worker
kubemono token create

# 加 master / 控制面（会轮换 certificateKey）
kubemono token create --role master
```

注意：

- `token create` 只签发凭证，不重跑安装 phase，不影响已运行节点；
- **worker 凭证不会碰 certificateKey**，所以不会让已有的 master 命令失效；
- **master 凭证会轮换 certificateKey**，已签发的 master join 命令立即失效；已有的 worker 命令不受影响；
- 命令不是一次性的：同角色的多个节点可以在 2h 有效期内复用同一条命令；过期后残留内容不可用；
- 如果所有 master 都不可用，先恢复一台 master 的 `admin.conf` 和 PKI，再执行签发。

### 3.6 日常变更

kubemono 不维护集群内配置副本，也没有后续的配置分发链路。变更流程是：

```bash
# 1) 在准备机上改 cluster.yaml，并做严格校验
kubemono config validate -f cluster.yaml

# 2) 把新 cluster.yaml 拷到目标节点，幂等重跑已安装节点的 phase
kubemono node repair -f cluster.yaml

# 3) 证书到期时在节点上续期（本机 PKI，不依赖 apiserver）
kubemono cert check-expiration
kubemono cert renew -f cluster.yaml --dry-run
kubemono cert renew -f cluster.yaml
```

`node repair` 按与 `init/join` 相同的 phase 表重跑本机安装：主机基线、运行时、
证书、kubeconfig、kubelet、etcd/控制面（master）、网络与附属组件都是幂等的，
已完成的部分会被 `Check` 跳过。集群级对象（kube-router、kube-proxy、CoreDNS）由 master 上的
`node repair` 一并收敛；这些 phase 需要 apiserver，不可达时 `node repair` 会在本机
phase 之后失败并给出客户端错误（不会假装成功）。

改动面很小时不必重跑全表，用 `--only` 把修复收敛到本机计划内的某个 phase
（顺序按计划而不是命令行）：

```bash
# 改了 cluster.yaml 的 master 列表后，只把 worker 的本地代理推下去
kubemono node repair -f cluster.yaml --only local-proxy
```

修复前想先看会改什么，用 `--diff`（只读：打印缺失/内容/权限差异，Secret 只报
状态不打印内容）：

```bash
kubemono node repair -f cluster.yaml --diff
```

`--timeout 30m` 限制整轮修复时长（0 表示不限制）；`--diff` 与 `--only` 不能同时使用。
`--only` 只接受本机计划里存在的 phase（worker 上没有 `etcd`，master 上没有
`local-proxy`），`init/join` 不支持；带 `--only` 时不会重跑 `postInstall` 钩子。
修复会先把内容与磁盘比较一次，只有真的不同才写文件/重启组件——所以清理生成
文件里的注释这类一次性变更，只会在第一次修复时触发重启。带 `--only` 的部分运行
不会更新 `desired` 快照与 `cluster.yaml` 缓存（避免把没执行的文件写成“已应用”）。

> 节点增减：新增节点按 §3.5 `join`；移除节点在 master 上先 `kubectl drain`，
> 再在目标节点执行 `kubemono reset --leave`，最后从 `cluster.yaml` 删除该节点条目。

### 3.7 版本变更

**appliance 模式：换版本 = 新建集群 + 迁移工作负载。** kubemono 不支持原地升级，`kubernetesVersion` 只在新建集群时生效；已有集群要改它只能按新配置重装。

同一 kubemono 版本内，`kubernetesVersion` 可以在**新建集群**时固定任意 Kubernetes 版本（见 §8）；与 BOM 默认版本不同时只打印 warning，兼容性由操作者负责。已有集群要换这个值，仍然只能新建集群后迁移。

对外口径：理论兼容 `v1.31` 及以上；当前 BOM 基线与验证上限为 `v1.37.x`，更高版本未验证，不承诺上限。flag 级差异优先用 `extraArgs` / `node.kubelet.configPatch` 调整，但它们不能弥补上游已移除的 flag、配置 schema 变化或 etcd/CoreDNS/kube-router 等非 k8s 组件的版本差异。

这样做是有意的产品边界，不是缺失功能：生产集群很少变动控制面版本，跨大版本通常也走“新建集群 + 迁移”，原地升级的复杂度与回滚风险不划算。

### 3.8 退出 / 重置

```bash
kubemono reset --leave      # 退群（cordon、移除 etcd 成员、删除 Node）后重置本机
kubemono reset --dry-run    # 只预览将删除/还原什么
kubemono reset --keep-data  # 保留 etcd/kubelet 数据目录
kubemono reset --purge -y       # 彻底清理：二进制/CNI/containerd 数据/配置/服务/物料
```

`reset` 会先把 PKI 与 `/etc/kubernetes/encryption`（若存在）备份到 `/usr/local/kubemono/backup/<timestamp>/`，备份失败即中止删除；加密集群的 encryption key 与 PKI 同等重要。reset 只停止/禁用服务并删除配置与 unit，**不删除** `/usr/local/bin`、`/opt/cni/bin` 下的运行二进制，也不删除 `/usr/local/kubemono/artifacts` 物料。

`reset --purge` 是显式的彻底清理：额外删除 `/usr/local/bin` 中 BOM 声明的二进制（含 kubemono 自身，如果在 `/usr/local/bin`）、`/opt/cni/bin` 中 kubemono 安装的插件、`/etc/containerd`、containerd 数据目录、`/usr/local/kubemono` 与运行时 unit。PKI/加密配置先备份到 `/usr/local/kubemono/backup/<timestamp>/`；成功 purge 后备份随根目录一起删除，失败时保留用于恢复。`--purge` 不能与 `--keep-data`/`--keep-hostinit` 同用；`-y/--assume-yes` 只负责跳过确认。

---

## 4. 命令一览

| 顶层 | 子命令 | 作用 |
|---|---|---|
| `init` | — | 初始化集群（不生成 join 凭证） |
| `token` | `create` | 按角色签发 join 凭证并打印 join 命令（默认 worker） |
| `join` | — | 以加入者身份安装本节点 |
| `config` | `default` `validate` | 配置模板与严格校验 |
| `node` | `repair` | 幂等重跑已安装节点的 phase（应用新配置的唯一入口） |
| `cert` | `check-expiration` `renew` `show` `timer` | 证书到期扫描、本机叶子证书续期（可用 `--cert <id>` 只续期指定证书）、只读查看证书内容、可选自动续期 timer |
| `bundle` | `pack` `list` `load` `verify` | 造包、物料查看、归档导入、齐备性校验 |
| `pki` | `issue` | 用已有 CA 离线签发叶子证书（不创建 CA、不自动写入 /etc/kubernetes） |
| `reset` | —（`--leave`） | 退群 + 本机重置 |
| `status` | — | 只读汇总本机收敛状态（身份/release/desired 漂移/phase 结果/待重启/证书/unit/Node Ready） |
| `whoami` | — | 只读打印本机身份（配置名/NodeName/地址/角色/集群 ID） |
| `version` | — | 版本与固定组件版本 |

造包与消费都在同一个二进制：`kubemono bundle pack` 产出归档，`kubemono bundle load <archive>` 按归档 meta 导入物料库；`list`/`verify` 只读。`make dist` 只是在构建机上跑同一条 pack 命令。

每条命令的使用场景、必填/可选参数与行为边界见 [`docs/cli-reference.md`](docs/cli-reference.md)；配置字段见 [`docs/cluster-config.md`](docs/cluster-config.md)。

---

## 5. 能力矩阵

| 领域 | 能力 |
|---|---|
| 集群形态 | 单/多 master、本地 etcd、external etcd（用户自行部署）、master/worker join、NAT |
| 主机基线 | 显式内核模块与 sysctl 清单（完整可编辑，不是隐藏默认值 + extra）、集群目录、hook；文件/sysctl 变更 journal 可回滚。不改主机名、limits、swap、SELinux、时区、NTP、防火墙、`/etc/hosts` |
| 运行时 | containerd 安装/配置、registry mirror/TLS、ctr（随 containerd 默认安装）；custom CRI 接入（不写运行时配置，CNI 插件仍按 BOM 安装） |
| 命令行 | master 随控制面安装 kubectl；不安装额外 CLI，ctr 由 containerd 提供 |
| 控制面 | apiserver/kcm/scheduler 以 systemd unit 运行；etcd learner 加入/提升/移除 + quorum 检查 |
| PKI | 自签 CA（RSA2048/ECDSA-P256）、三个信任域外部 CA（kubernetes/front-proxy/etcd，支持证书链）、SAN、到期检查、逐台/按证书续期、`cert show`、`pki issue`；不支持 CA 轮换 |
| 引导安全 | bootstrap token（TTL）、cluster-info JWS、CA pin（`--discovery-token-ca-cert-hash`）、AES-256-GCM 证书包、certificateKey |
| 网络 | kube-router（CNI + BGP/overlay）、kube-proxy DaemonSet（Service 代理，默认 IPVS，可配 iptables/nftables）、`plugin=none`；worker 本地 haproxy。上游 CNI 插件默认全量安装（18 个，与 runtime/plugin 无关），`plugin=none`/自定义网络可直接用 |
| 附加组件 | CoreDNS（受管块合并） |
| 集群安全 | encryption-at-rest、审计、最小 RBAC、admin/super-admin |
| 版本变更 | appliance 模式：换版本 = 新建集群 + 工作负载迁移；`kubernetesVersion` 只在新建集群时生效 |
| 离线物料 | `meta.json` + 二进制 + 可选 OCI layout；归档级摘要 + 逐文件 sha256；`load`/`verify` 消费 |
| 运维可观测 | `status` 只读汇总身份/release/desired 漂移/phase 结果/待重启/证书/unit/Node Ready（table/json/yaml） |

---

## 6. 架构

```
cmd/kubemono                     唯一的发布入口（main/phase 空导入）
  └─ internal/cli             节点命令树/flag 解析/结果渲染
       └─ internal/<domain>  业务逻辑（install、artifact…）
            └─ internal/phase 执行单元（Name/Applies/Check/Run）
                 └─ ports     systemctl/文件/HTTP/K8s client 均可注入

internal/config              cluster.yaml/node.yaml 类型、默认值与校验
internal/cli/bundle           kubemono bundle pack/load/list/verify（造包与消费）
  └─ internal/pack            组装物料（组件下载/镜像布局/版本覆盖）
       └─ internal/artifact/release  归档写出与平台校验
```

- `internal/config`：集群/节点配置类型、默认值与校验；只描述 kubemono 自己的 YAML，不是 Kubernetes API 对象。
- `internal/phase`：14 个 canonical phase（`CanonicalOrder`），按依赖顺序执行；状态写入 `/usr/local/kubemono/state/*.json` 供排障与审计；续跑由各 phase 的 `Check` 保证。
- `internal/render`：纯函数渲染 + desired 快照；所有受管文件经 `fsutil.WriteFileAtomic` 落盘。
- `internal/artifact`：物料格式、归档解包、落盘与齐备性校验；`internal/artifact/release` 是造包实现，由 `internal/pack` 调用（唯一入口 `kubemono bundle pack`）。

### 6.1 集群内 kubemono 对象

| 对象 | 用途 |
|---|---|
| `kube-system/kubemono-cluster` ConfigMap | 集群身份与状态：clusterID、release、CNI/service CIDR、plugin、serviceProxy、etcd 类型 |
| `kube-public/cluster-info` ConfigMap | join 入口：CA kubeconfig、clusterID、release，以及每个 bootstrap token 的 JWS 签名 |
| `kube-system/kubemono-certs` Secret | master join 用的 AES-256-GCM 加密证书包（CA/SA 等物料） |
| `kube-system/bootstrap-token-*` Secret | join 用的 bootstrap token（标准 Kubernetes 类型，kubemono 注册） |
| `kube-system/kube-router` ConfigMap | kube-router 的 CNI 参数/ServiceAccount kubeconfig |
| `kube-system/kube-proxy` ConfigMap | kube-proxy 的 KubeProxyConfiguration/ServiceAccount kubeconfig |
| `kube-system/coredns` ConfigMap | CoreDNS Corefile；只有 `# BEGIN/END kubemono managed block` 由 kubemono 管理 |

以下**不是** kubemono 专有对象：`kube-root-ca.crt`（每个命名空间的根 CA 副本）、`extension-apiserver-authentication`、`kube-apiserver-legacy-service-account-token-tracking`。

---

## 7. 离线物料

发布形态只有一个二进制 `kubemono`；造包（`kubemono bundle pack`）与消费（`bundle load`）都在它里面，`make dist` 只是在构建机上调用同一条命令。

```bash
# 构建机：默认轻量包（不含镜像）；加 --images 生成完整包
kubemono bundle pack -f cluster.yaml                     # → ./dist/kubemono-<release>-linux-amd64-lite.tar.zst
kubemono bundle pack -f cluster.yaml --images            # → ./dist/kubemono-<release>-linux-amd64.tar.zst
kubemono bundle pack -f cluster.yaml --bom ./bom.yaml    # 自定义组件列表/获取 URL

# 严格离线：构建机出包，节点只消费
make dist                       # 默认轻量 amd64；dist 已依赖 build-linux，ARCHS=amd64,arm64 IMAGES=1 出完整双架构
kubemono bundle verify /mnt/usb/kubemono/kubemono-...tar.zst
kubemono bundle load /mnt/usb/kubemono/kubemono-...tar.zst

# 其它
kubemono bundle list -f cluster.yaml                    # 需要什么、本地有没有
kubemono bundle list /mnt/usb/kubemono/kubemono-...tar.zst    # 归档里有什么
kubemono bundle verify -f cluster.yaml                  # 本机物料齐备性
```

| 命令 | 用途 |
|---|---|
| `pack` | 唯一造包入口：按 BOM 获取物料并写出归档；`-f` 控版本，`--bom` 换组件/URL，默认 `linux/amd64` 轻量包，`--images` 出完整包，`--refresh` 强制刷新组件缓存，`--offline` 只用缓存 |
| `list` | 按 cluster.yaml/BOM 列出组件、版本与本地状态；`list <archive>` 只读归档 meta |
| `load` | 从本地归档导入物料库；release/arch/images 以 meta 为准，不联网、不构建 |
| `verify` | 校验归档内容（主用法）或本机物料：存在、大小、权限、sha256、ELF 架构、OCI layout |

- **唯一根目录**：`/usr/local/kubemono/` 下集中 `node.yaml`、`cluster.yaml`、`desired/`、`artifacts/`、`backup/`、`state/`、`hostinit.json`；用各命令的 `--data-dir` 可以把整个目录换到其他位置（不是 root 继承的全局 flag）。
- 物料布局：`/usr/local/kubemono/artifacts/<release>/` 下 `meta.json`、`bin/<category>/<name>`、可选 `images/`；kubemono 二进制不在这里，单独发布。
- **运行二进制不在物料目录**：安装 phase 把组件二进制直接复制到 `/usr/local/bin/<name>`，CNI 插件平铺到 `/opt/cni/bin/<name>`；没有 `/usr/local/kubemono/bin`、没有版本目录、没有二进制软链。systemd unit 直接引用这些路径，因此安装完成后删除 `/usr/local/kubemono` 不会影响运行中的集群（`node repair` 仍需要物料重新复制）。
- 归档以 `meta.json` 开头：`load` 只解压一遍就能先拿到清单，再逐文件校验并落盘。
- 完整性：归档旁的 `.sha256` 作为传输摘要；`meta.json` 里的逐文件 sha256 在 `verify`/`load` 时校验。归档与物料以 `meta.json` 为权威。
- **Kubernetes patch 用单个 `kubernetesVersion` 控制**：例如 `kubernetesVersion: v1.37.4`，全部 k8s 组件（含 kube-proxy）整组跟随它，不能逐个指定。生效 BOM 的 `kubernetes`/`release` 同步改写为 `v1.37.4` / `v1.37.4-km.1`。任意版本都允许，与默认版本不同时只打印 warning。
- **物料内只有二进制**：打包时解开组件压缩包，只保留 `bin/<category>/<name>`；CNI 默认保留上游归档里的全部 18 个插件（`bin/cni/`）。节点 `bundle load` 后按 BOM 白名单逐个复制到 `/opt/cni/bin`，不解压任何组件压缩包。
- **镜像可选（默认关）**：默认生成 `-lite` 轻量包，不拉取也不携带 OCI layout；加 `--images` 才生成完整包并允许拉取缺失镜像。load 只落盘，镜像导入仍由 runtime phase 在 containerd 就绪后执行，不需要 `ctr`。
- **目录边界**：`--cache-dir` 是下载缓存根目录，实际按 `<cache-dir>/<release>/<component>/<arch>/<source-hash>` 隔离；`--out-dir` 只接收最终归档与 `.sha256`，输出目录不能与该 release 的缓存目录互相包含；组装暂存目录在系统临时目录，不写进 `--out-dir`。缓存 key 含展开后的 source URL 哈希，改 BOM URL/版本不会命中旧文件；`--refresh` 可强制重下组件。
- **归档不含 `kubemono` 自身**：kubemono 二进制是独立发布物（`make build-linux` 产物），物料归档只包含 BOM 声明的组件与可选 OCI layout；BOM 出现 `kubemono` 组件会被拒绝。目标架构用 `--arch linux/amd64`（默认）或 `linux/arm64`。

---

## 8. 安全模型

- **引导**：cluster-info 由 bootstrap token secret 做 JWS 签名；join 命令携带 CA pin（`--discovery-token-ca-cert-hash`）；证书包 AES-256-GCM 加密，certificateKey 只用于 master 加入。
- **纯 worker 证书最小化**：worker 只从加入流程拿到 `ca.crt`，由 TLS Bootstrap 换取自己的 kubelet 凭据；worker 使用 `--control-plane`/`--certificate-key` 会被拒绝；TLS Bootstrap 完成后删除含明文 token 的 `bootstrap-kubelet.conf`。
- **token 传递**：join 支持 `--token-file` / `--certificate-key-file`，避免凭证出现在 argv 与 `/proc/<pid>/cmdline`。
- **主机密钥**：私钥 0600、目录 0700、原子写；CA 缺失/不完整时拒绝自动重建。
- **集群内**：encryption-at-rest（secretbox，密钥不出 master）、审计、最小 RBAC。
- **离线物料**：归档级 sha256 + 逐文件 sha256（可选）+ 安全解包（拒绝绝对路径/`..`/软硬链）；物料以 `meta.json` 为权威。

---

## 9. 运维模型与边界

**没有**：常驻进程、自动漂移修复、默认开启的证书自动续期（可选 systemd timer）、配置回滚/历史、etcd 备份/恢复命令、`doctor` 诊断建议、CA 轮换、原地版本升级、Windows 节点、IPv6/dual-stack（validation 会明确拒绝 IPv6 地址、CIDR、clusterDNS 与 externalEndpoints）。只读状态汇总用 `kubemono status`。

**因此**：

- 同一 `--data-dir` 的写操作（init/join/repair/cert renew/reset）由 `state/kubemono.lock` 互斥；备份只保留最近 10 份，退役节点请用 `reset --purge` 清掉 PKI 备份。
- 配置改了要逐台 `node repair`；证书到期默认逐台 `cert renew`（可用 `--cert apiserver,etcd-server` 只续期指定证书），也可以在 master 上安装一次 `kubemono cert timer install`，由 systemd timer 每天执行。
- 版本变更用新版本物料新建集群，工作负载迁移与切换节奏由运维或 CI 负责。
- etcd 备份用 `etcdctl snapshot save`，恢复用 `etcdutl`；节点/工作负载排障用 `kubectl`、`systemctl`、`journalctl`。

---

## 10. 开发

```bash
make build        # 本机调试二进制
make build-linux  # 发布二进制（linux/amd64 + linux/arm64）
make dist         # 物料归档（默认轻量；ARCHS/IMAGES 见 §3.1）
make fmt          # gofmt 检查
make vet          # GOOS=linux go vet ./...
make test         # 单元测试
make e2e-local    # 本地 CLI 链路：bundle load/verify/list + pack 集成
make test-race    # 竞态检测
make tools        # 安装固定版本的 deadcode / golangci-lint 到 bin/（CI 前置）
make lint         # golangci-lint v2（GOOS=linux，固定版本见 Makefile）
make check        # fmt + vet + test
make ci           # 提交门禁：fmt/vet/test-race/build-linux + lint/死代码/边界/文档链接（需先 make tools）
```

要求 Go 1.27。单元测试在 macOS 上也能跑（特权操作通过端口注入 fake）；Linux 专用实现在 `*_linux.go`，需要真机或 VM 验证。

### CI（GitHub Actions）

[`.github/workflows/ci.yml`](.github/workflows/ci.yml) 在 push 到 `main`、所有 PR 与手动触发时
跑一套必要门禁：

1. `make tools`：按 Makefile 固定版本编译 golangci-lint 与 deadcode（缓存在 `bin/`，命中缓存则跳过）；
2. `make ci`：gofmt、`GOOS=linux` 的 `go vet`、`-race` 单元测试、linux/amd64 + arm64 交叉构建、
   golangci-lint、死代码、二进制边界与文档链接检查。

CI **不跑**真实集群的 init/join/多 master 端到端：那需要多台 Linux 主机、root 与网络环境，
托管 runner 不具备条件，用 mock 假装覆盖也没有意义。发布前仍由人工清单把关。

---

## 11. 文档

- [`docs/cluster-config.md`](docs/cluster-config.md)：`cluster.yaml` 全字段、默认值、约束、可自定义项与数据目录
- [`docs/cli-reference.md`](docs/cli-reference.md)：全部命令的使用场景、必填/可选参数、执行流程与排错
- [`docs/bom.md`](docs/bom.md)：BOM 字段、版本/下载地址/镜像规则与 `bundle pack` 的配合
- [`examples/cluster.yaml`](examples/cluster.yaml)：带级别标注的完整配置示例

---

## License

MIT，见 [`LICENSE`](LICENSE)。
