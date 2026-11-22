# cluster.yaml 完整配置参考

本文覆盖 `cluster.yaml` 当前可写的**全部字段**，包括 `kubemono config default` 不会展开的约束、默认值、相互关系和常见坑。

- 根类型：`Cluster`；配置不带 Kubernetes 风格的 `apiVersion`/`kind` 信封。
- 加载方式：严格 YAML，**未知字段直接拒绝**
- 校验：`kubemono config validate -f cluster.yaml`
- 生成起点：`kubemono config default > cluster.yaml`

> 命令行怎么用（每条命令的使用场景、必填/可选参数、执行流程）见 [`cli-reference.md`](cli-reference.md)；
> 组件版本、下载地址与 `bundle pack` 的物料规则见 [`bom.md`](bom.md)。

**怎么读本文**：§1–§7 按段解释字段；§8 是「能改什么、怎么生效」的总览；
§9 汇总 `extraArgs`/`configPatch`；§10 是校验与常见坑；§11 是可直接复制的完整骨架。

```bash
kubemono config validate -f cluster.yaml
```

> `config default` 输出的是“补齐默认值后的有效配置”，不是全部可写字段的说明。本文以代码中的 `internal/config` 类型、默认值和校验为准。

---

## 1. 顶层字段

**必填的只有一样**：`cluster.hosts`（节点清单）。其余顶层段
（`cluster.hooks`、`node`、`controlPlane`、`network`、`addons`、`images`）都可以整段省略，
省略即由 `SetDefaults` 填入内置默认值。`kubemono config default` 输出的是「补齐默认值后」
的配置，所以大部分段都以显式字段出现——那是展开结果，不是书写要求。

### 最小可用配置

```yaml
cluster:
  hosts:
    - name: m1
      address: 10.0.0.11
      roles: [master, worker]
```

这份配置可以直接通过 `kubemono config validate`：`node` / `controlPlane` / `network` /
`addons` / `images` 全部按默认值展开（podCIDR `10.244.0.0/16`、serviceCIDR `10.96.0.0/12`、
clusterDNS `10.96.0.10`、containerd、kube-router、kube-proxy、CoreDNS 等）。

### 全部顶层字段

```yaml
kubernetesVersion: v1.37.4        # 可选；省略用 BOM 内置版本
cluster:                          # 必填（实质是 cluster.hosts）
  hosts:                          # 必填：至少一个节点，且至少一个含 master
    - name: m1
      address: 10.0.0.11
      roles: [master, worker]
  hooks: {}                       # 可选
node: {}                          # 可选
controlPlane: {}                  # 可选
network: {}                       # 可选
addons: {}                        # 可选
images: {}                        # 可选
```

| 字段 | 类型 | 是否必填 | 省略时 |
|---|---|---|---|
| `kubernetesVersion` | string | 可选 | 使用内置 BOM 的 Kubernetes 版本 |
| `cluster` | object | **必填**（实质是 `cluster.hosts`） | 报错 `cluster.hosts: Required value: at least one host is required`；节点拓扑无法推导，没有默认值 |
| `cluster.hosts` | []object | **必填** | 同上；每项至少写 `name`、`address`、`roles`，且至少一个节点含 `master`（见 §2.1） |
| `cluster.hooks` | object | 可选 | 不执行任何钩子 |
| `node` | object | 可选 | 主机基线、containerd、kubelet 全用默认值（§3） |
| `controlPlane` | object | 可选 | PKI、本地 etcd、控制面参数全用默认值（§4） |
| `network` | object | 可选 | kube-router、默认 CIDR、kube-proxy 开启（§5） |
| `addons` | object | 可选 | 安装 CoreDNS，2 副本（§6） |
| `images` | object | 可选 | 全部使用 BOM 内置镜像与版本（§7） |

顶层可选段写成 `{}` 与整段省略等价；但**段内字段**写成空值不一定等价——例如
`node.host.kernelModules: []` 表示「一项都不加载」，而省略该字段表示「按网络配置生成默认清单」
（§3.1）。

### 为什么看起来「每段都必填」

- `config default` 会把已填的默认值展开成显式字段（`omitempty` 的空段不出现），看起来每段都在
  只是展开结果；
- 加载流程是「严格解析 → `SetDefaults` 补默认 → `Validate`」，省略的段在校验前已经是默认值，
  不会因为没写 `node` / `network` 报错；
- 但**段可省略 ≠ 段内所有字段可省略**：下面这些是条件必填，默认值没法替用户决定。

| 位置 | 条件必填 | 见 |
|---|---|---|
| `cluster.hosts[]` | `name`、`address`、`roles`；`nodeName: address` 时 `address` 不能为空 | §2.1 |
| `node.runtime.custom` | 该段任一字段非空时：`criSocket`、`serviceName` 必填 | §3.2 |
| `node.runtime.containerd.registries.configs.<host>` | `username` / `password` 必须成对 | §3.2 |
| `controlPlane.pki.externalCA` / `externalFrontProxyCA` / `externalEtcdCA` | `certFile` / `keyFile` 必须成对 | §4.1 |
| `controlPlane.etcd.external` | 该段任一字段非空时：`endpoints`、`caFile`、`certFile`、`keyFile` 必填 | §4.2 |
| `images.components.<component>` | 写了 key 就必须写完整的镜像引用 | §7.1 |

> 实践建议：只写要改的段，其余留给默认值；需要「全字段参照」时再 `kubemono config default`，
> 两种写法都能通过 `config validate`。

### `kubernetesVersion`

```yaml
kubernetesVersion: v1.37.1
```

- 它是全部 Kubernetes 组件（含 kube-proxy）的**唯一版本入口**：kube-apiserver、kube-controller-manager、kube-scheduler、kubelet、kube-proxy、kubectl 整组跟随；
- 只要求 `vX.Y.Z` 稳定版格式；
- 任意版本都允许，不限制最低 patch、小版本或兼容范围；
- 兼容性口径：理论兼容 `v1.31` 及以上，不承诺上限；当前 BOM 基线与验证上限为 `v1.37.x`，更高版本未验证；
- `extraArgs` / `node.kubelet.configPatch` 只能调整目标版本仍存在的 flag/config 字段；上游已移除的 flag、kube-proxy 配置 schema 变化，以及 etcd/CoreDNS/kube-router 等非 k8s 组件的版本窗口，不在覆盖范围内；
- v1.31–v1.34 已 EOL，仅作理论兼容；生产建议使用上游仍支持的 minor；
- 非空且与 BOM 不同时只打印 warning；生效 BOM 的 `kubernetes` 与 `release` 会同步改写，物料归档与 `meta.json` 都使用新 release；
- 修改前必须准备对应 patch 的物料归档；版本变更通过新建集群完成，不做原地升级；
- 不能再在 `images.componentVersions` 里单独固定 k8s 组件。

只能在新建集群时设置。

---

## 2. `cluster`

```yaml
cluster:
  hosts: []    # 至少一个节点；整段省略或留空都会校验失败（见 §1）
  hooks: {}
```

### 2.1 `cluster.hosts[]`

```yaml
cluster:
  hosts:
    - name: m1
      nodeName: hostname
      address: 10.0.0.11
      matchAddresses: [192.168.1.11]
      roles: [master, worker]
      labels:
        zone: a
      taints:
        - "dedicated=infra:NoSchedule"
```

| 字段 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `name` | string | 无 | 配置身份键，必填；DNS-1123 subdomain；集群内唯一 |
| `nodeName` | string | `hostname` | 实际 Kubernetes Node 名 / etcd 成员名 / 证书 CN |
| `address` | string | 无 | 节点声明地址，必填；IPv4，集群内唯一（IPv6 当前不支持，validation 会拒绝） |
| `matchAddresses` | []string | `[]` | 本机额外匹配地址；用于多网卡/NAT/地址漂移 |
| `roles` | []string | 无 | `master` / `worker` 的任意非空组合；不能重复 |
| `labels` | map[string]string | `{}` | 应用到 Node 的标签；key 不能为空 |
| `taints` | []string | `[]` | kubelet `register-with-taints` 格式，如 `key=value:NoSchedule` |

#### `name` 与 `nodeName` 的区别

- `name` 只是 cluster.yaml 里的身份键，用于配置引用、`--node`、join 校验等；
- 真正的 Node 名、etcd 成员名、证书 CN 由 `nodeName` 决定；
- `nodeName` 的取值：
  - `hostname`（或留空）：使用本机 hostname；
  - `address`：使用 `address`；
  - 其他值：原样作为节点名。
- 多 master 集群建议显式写 `nodeName`（`address` 或固定名字），不要依赖远端机器 hostname 才能推导 etcd/证书身份；
- `nodeName` 为 IPv6 地址时会被拒绝（Kubernetes NodeName 不接受 IPv6 字面量）；
- IPv6 节点地址、CIDR、clusterDNS 与 externalEndpoints 当前整体不支持：
  etcd URL、CNI 与 dual-stack 尚未实现，validation 会明确报错，而不是留到运行期失败；
- 显式 `nodeName` 必须是合法 DNS-1123 subdomain，且集群内唯一。

#### `address` / `matchAddresses`

- `address` 是集群内识别该节点的声明地址，必须能通过 `netip.ParseAddr`；
- `matchAddresses` 用于本机实际地址与声明地址不同（NAT、多网卡、浮动地址）的场景；
- 所有地址（含 match）会参与重复检测，且不能落在 `network.podCIDR` / `network.serviceCIDR` 内；
- 如果 `nodeName: address`，`address` 不能为空。

#### `roles`

| 写法 | 含义 |
|---|---|
| `[master]` | 纯控制面：不装 kubelet/containerd/CNI，不注册 Node |
| `[master, worker]` | 混合节点：控制面 + 工作负载；默认模板使用这个组合 |
| `[worker]` | 纯工作节点 |

- 至少一个节点必须含 `master`；
- 纯 master 没有 kubelet，因此不会运行 kube-router 等 DaemonSet；混合节点和 worker 才会；

#### `labels` / `taints`

```yaml
labels:
  topology.kubernetes.io/zone: az1
  node-role.example.com/infra: ""
taints:
  - "dedicated=infra:NoSchedule"
  - "temporary=true:PreferNoSchedule"
```

- labels 与 taints 都会写入 Node 对象；kubemono 还会维护自己的
  `node-role.kubernetes.io/control-plane` 等标签；
- taints 同时进入 kubelet 的 `--register-with-taints`，保证注册时就生效；后续的
  新增、改值由 `mark-control-plane` 对账，不碰云厂商/CNI 的 taint；
- `mark-control-plane` 默认使用本机 kubelet 凭据（`kubelet.conf`），受
  NodeRestriction 限制不能修改自身 taints：修改 taints 时需要显式
  `--kubeconfig /etc/kubernetes/admin.conf`，否则会明确报错（不会静默不生效）；
- 这两个字段做轻量校验：label key 非空；taint 的 key 非空且 effect 只能是
  `NoSchedule`/`PreferNoSchedule`/`NoExecute`（省略 effect 时按 `NoSchedule`）。
  Kubernetes 的完整合法性仍由 apiserver/kubelet 最终检查；
- 从配置里删掉一个 label/taint 不会主动从 Node 移除（避免误删他人写入的项），
  需要时用 `kubectl label` / `kubectl taint` 手工清理。

### 2.2 `cluster.hooks`

在所有节点上执行的自定义 shell 钩子：

```yaml
cluster:
  hooks:
    preHostInit:
      - "echo pre-host-init >> /var/log/kubemono-hooks.log"
    postRuntime:
      - "systemctl restart my-agent.service"
    postInstall:
      - "/usr/local/bin/register-node.sh"
```

| 字段 | 执行时机 |
|---|---|
| `preHostInit` | host-init 主机初始化之前 |
| `postRuntime` | containerd/custom runtime 就绪之后 |
| `postInstall` | 本节点安装完成、desired 快照写入之后 |

注意事项：

- 通过 `sh -c` 执行，每个钩子超时 10 分钟；
- 以 root 执行，失败会中止当前安装/阶段；
- kubemono **不会回滚钩子的副作用**；钩子必须自己保证幂等和可重入；
- `preHostInit` 在 host-init 的内核模块、sysctl、目录创建之前执行；host-init 已完成且没有漂移时不会重跑；
- 同一组钩子在所有执行到对应 phase 的节点上运行；需要按节点区分时，在脚本内用 `hostname` 等自行判断；
- 不注入 kubemono 专用环境变量，也不保证工作目录；脚本内使用绝对路径；
- `postInstall` 在每次完整 `init`/`join`/`node repair` 成功后执行；带 `--only` 时跳过；
- 不要在钩子里做交互式操作；
- 命令内容会进入集群配置/日志，避免写入明文密码；
- 实际影响由钩子内容决定。

---

## 3. `node`

```yaml
node:
  host: {}
  runtime: {}
  kubelet: {}
```

### 3.1 `node.host`

| 字段 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `kernelModules` | []string | 按网络配置生成（见下） | **完整**内核模块清单；不是追加 |
| `sysctls` | map[string]string | 见下 | **完整**内核参数清单；不是追加 |

kubemono 只做 Kubernetes 必需的主机基线：内核模块、sysctl、集群目录与 hooks。
主机名、`/etc/security/limits.d`、swap、SELinux、时区、NTP、防火墙、`/etc/hosts`
都不修改；需要这些主机策略时用 `cluster.hooks` 或镜像层完成。

两个字段都可以省略：省略时由 `SetDefaults` 按当前网络配置生成默认值；
一旦写出来，就是**整份替换**，不与默认值合并（写 `[]` / `{}` 表示一项都不做）。

#### `kernelModules`

```yaml
kernelModules:
  - overlay
  - br_netfilter
  - nf_conntrack
  - ip_vs
  - ip_vs_rr
  - ip_vs_wrr
  - ip_vs_sh
  - ipip
```

- 这是**完整清单**，kubemono 只加载这里列出的模块；没有隐藏默认值 + extra 追加；
- 默认值按当前网络配置展开：基础 `overlay`、`br_netfilter`、`nf_conntrack`；kube-proxy
  启用时，`mode=ipvs`（默认）追加 `ip_vs`、`ip_vs_rr`、`ip_vs_wrr`、`ip_vs_sh`，
  `mode=nftables` 追加 `nf_tables`；`plugin=kube-router` 且 `enableOverlay=true` 时追加 `ipip`；
- 逐个执行 `modprobe`，同步写入 `/etc/modules-load.d/kubemono.conf`，重启后再次加载；
- 空列表表示一项都不加载，并写空文件清掉上一次的受管列表；
- 只接受模块名，不支持 `modprobe` 参数/options；
- 模块加载失败不会中止 host-init：前台输出警告后继续，持久化列表仍会写入。

> 改 `network.kubeProxy.mode`、`network.plugin` 或 `enableOverlay` 后，默认清单不会自动重算：
> `config default` 生成的是当时配置的清单，已有 cluster.yaml 里请同步增删模块。

#### `sysctls`

```yaml
sysctls:
  net.ipv4.ip_forward: "1"
  net.bridge.bridge-nf-call-iptables: "1"
  fs.inotify.max_user_instances: "8192"
  fs.inotify.max_user_watches: "524288"
```

- 这是**完整 map**，kubemono 只设置这里列出的键；不与隐藏默认值合并；
- 写入 `/etc/sysctl.d/99-kubemono.conf`，重启后由 systemd-sysctl 重放；
- 运行时设置失败会警告并继续，持久化文件仍会写入；
- 空 map 表示一项都不设置，并写空文件清掉上一次的受管内容；
- 值必须写成字符串，避免 YAML 把数字/布尔转成非字符串；
- key 必须带命名空间（含 `.` 或 `/`），只允许字母、数字、`_`、`-`、`.`、`/`，
  且不能含 `..`、首尾分隔符或空段（这些键会被映射到 `/proc/sys` 下）；
- 值不能含换行或 NUL（会破坏 `/etc/sysctl.d` 的行结构）。

### 3.2 `node.runtime`

```yaml
node:
  runtime:
    containerd: {}
    # custom: {}
```

| 字段 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `containerd` | object | | kubemono 托管 containerd 的配置 |
| `custom` | object | | 用户自备 CRI 的配置 |

没有 `type`：`custom` 任一字段非空即使用 custom，否则默认 containerd。
两者都配置时以 custom 为准，`containerd` 段不生效。

#### `containerd`

| 字段 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `root` | string | `/var/lib/containerd` | containerd 数据目录 |
| `systemdCgroup` | bool | `true` | 使用 systemd cgroup 驱动 |
| `registries.mirrors` | map[string][]string | `{}` | 仓库 host → mirror URL 列表 |
| `registries.configs` | map[string]RegistryAuth | `{}` | 仓库 host → CA/认证配置 |
| `configPatch` | string | 空 | 内联 TOML，深度合并进生成配置 |

`registries.mirrors` / `registries.configs` 的键必须是域名或 IPv4（可带端口，如
`registry.example.com:5000`）：键会直接作为 `certs.d` 下的目录名，含 `/`、`..`
或空白的键会被校验拒绝。

`registries.configs` 的每个条目：

| 字段 | 类型 | 说明 |
|---|---|---|
| `caFile` | string | 私有仓库 CA 文件路径 |
| `insecureSkipVerify` | bool | 跳过 TLS 校验，仅测试/明确接受风险时使用 |
| `username` | string | 仓库用户名 |
| `password` | string | 仓库密码；会进入集群配置，注意明文风险 |

`registries.mirrors` 示例：

```yaml
registries:
  mirrors:
    registry.k8s.io: ["https://mirror.example.com"]
    docker.io: ["https://docker-mirror.example.com"]
  configs:
    registry.example.com:5000:
      caFile: /usr/local/kubemono/registry-ca.crt
      username: kubemono
      password: change-me
```

注意：

- mirror 的 key 是镜像仓库 host；
- `configs` 会渲染成 containerd `certs.d/<host>/hosts.toml`；mirror 的 TLS/认证用 mirror URL 的 host 作为
  key 单独配置（上游仓库的认证仍用仓库 host）；mirror 始终排在 `server` 兜底之前；
- 同一 host 同时有 mirror 与 `configs` 时，上游认证会挂到最后一个 host 上写入，不会丢；
- `username`/`password` 必须成对出现（只写一个会被 `config validate` 拒绝），
  渲染成 `[host."<url>".header]` 下的 `authorization = "Basic <base64>"`；
- 因此 `hosts.toml` 以 **0600** 落盘（其中含明文凭据），生产环境仍应尽量使用
  最小权限账号或外部凭据机制；
- `root` 改变需要迁移已有镜像数据；
- `systemdCgroup` 需要重启 containerd；
- `configPatch` 是 TOML 字符串（不是 map），`config validate` 会做语法解析；例如：

```yaml
configPatch: |
  [plugins."io.containerd.grpc.v1.cri".containerd]
    default_runtime_name = "runc"
```

- 覆盖 containerd 配置前确认字段属于当前 BOM 固定的 containerd schema。

#### `custom`

| 字段 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `criSocket` | string | 无 | 必填；CRI socket 路径，写入 kubelet |
| `cgroupDriver` | string | 空 | `systemd` 或 `cgroupfs`；**空表示 kubelet 使用 `systemd`**，必须与运行时实际值一致 |
| `serviceName` | string | 无 | 必填；systemd unit 名，kubemono 会 `enable`/`start`，回滚或显式动作时 `restart` |

```yaml
runtime:
  custom:
    criSocket: unix:///run/crio/crio.sock
    cgroupDriver: systemd
    serviceName: crio
```

注意：

- 使用 custom 时 kubemono 不安装运行时二进制，CRI 由用户托管；
  CNI 插件仍会按 BOM 安装到 `/opt/cni/bin`；
- `runtime` phase 用配置的 CRI socket 做就绪探测，不依赖外部 CLI；
  运行时未就绪会直接失败；
- `custom` 下 kubemono **不写运行时配置**，但**会安装 CNI 插件**；用 `plugin=kube-router` 时
  让运行时从 `/etc/cni/net.d` 读配置、从 `/opt/cni/bin` 找插件；如果自定义 CRI 配了
  别的 `bin_dir`，需要自行复制/软链；
- 自定义运行时的升级、证书、配置由用户负责；CNI 插件物料由 kubemono 安装；
- `bundle pack --images` 生成的镜像由 containerd 的 `ctr` 导入，custom CRI 下 kubemono
  不会自动导入（会明确报错）；请把 `artifacts/<release>/images` 导入你的 CRI，或使用轻量包；

前置条件（每台带 `worker` 角色的节点，`init`/`join` 之前就要满足）：

1. **systemd unit 已存在**：`systemctl status <serviceName>` 能看到它；kubemono 会
   `daemon-reload`、`enable`、`start` 这个 unit。
2. **CRI socket 已存在**：runtime phase 只检查 unit active + `criSocket` 文件，
   不会安装额外 CLI，也不会执行额外 CRI 请求；socket 不存在会一直等到超时。
3. **cgroup driver 一致**：`custom.cgroupDriver` 与运行时实际配置一致；留空表示
   kubelet 使用 `systemd`，运行时用 `cgroupfs` 时必须显式写 `cgroupfs`。
4. **CNI 目录约定**：`plugin=kube-router` 时 kubemono 只写
   `/etc/cni/net.d/10-kuberouter.conflist`，并假定运行时从 `/etc/cni/net.d` 读配置、
   从 `/opt/cni/bin` 找插件；其他 CRI 需要在自己的配置里对齐这两个路径。
5. **主机基线**：`host-init` 仍会加载内核模块、写 sysctl；不要因为换了运行时把它们关掉。

常见失败：

| 现象 | 原因与处理 |
|---|---|
| `CRI socket ... 在 60s 内没有应答` | unit 没启动、socket 路径写错、权限/SELinux；`journalctl -u <serviceName>` 与服务端配置一起看 |
| kubelet 反复重启，日志提示 cgroup 相关错误 | `custom.cgroupDriver` 与运行时不一致；systemd 运行时写 `systemd` |
| 节点 `NotReady`，日志 `cni plugin not initialized` 或 `failed to find plugin "bridge"` | 自定义 CRI 的 CNI `bin_dir` 没指向 `/opt/cni/bin`；建立软链或改 CRI 配置 |

变更边界：

- containerd → custom（或反向）**不能原地切换**，需要新建集群并迁移工作负载；
- 已经是 custom 时，改 `custom.*` 后重跑 `kubemono node repair`；但 runtime phase 的
  `Check` 在运行时健康时会判定“已完成”并跳过，需要重新执行时先
  `systemctl stop <serviceName>`，再 repair；
- 运行时的升级、镜像仓库、TLS、日志清理由用户负责。

命令用法与更多排错见 [`cli-reference.md`](cli-reference.md)（`init`/`join`/`node repair` 章节）。

### 3.3 `node.kubelet`

| 字段 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `rootDir` | string | `/var/lib/kubelet` | kubelet 数据目录（`--root-dir`） |
| `configPatch` | map[string]any | `{}` | 深度合并进 kubelet 配置 YAML |
| `extraArgs` | map[string]string | `{}` | kubelet CLI flag 透传 |

```yaml
kubelet:
  rootDir: /var/lib/kubelet
  configPatch:
    maxPods: 250
    evictionHard:
      memory.available: "500Mi"
  extraArgs:
    v: "2"
```

注意：

- `rootDir` 同时影响 `--root-dir`、`--config`、`--cert-dir` 与 kubelet 数据目录；修改需迁移已有数据；
- `configPatch` 在 kubelet 配置渲染后合并，嵌套 map 递归覆盖；
- 不做字段级 schema 校验，最终由 kubelet 启动时校验；
- `extraArgs` 的 key 不以 `-` 开头，value 不允许空白；同名 flag 覆盖 kubemono 默认 flag；
- 覆盖 `--config`、`--root-dir`、kubeconfig、cgroup 等关键项可能破坏 kubemono 的假设；数据目录请用 `rootDir`；
- `rootDir` 修改需迁移已有数据；`configPatch`/`extraArgs` 会重启 kubelet。

#### 核心数据目录

| 目录 | 配置字段 | 默认值 | 修改注意 |
|---|---|---|---|
| containerd | `node.runtime.containerd.root` | `/var/lib/containerd` | 先停 containerd、迁移数据，再改配置并 `node repair`；否则已有镜像/快照不可见 |
| kubelet | `node.kubelet.rootDir` | `/var/lib/kubelet` | 同时决定 `--root-dir`、`--config`、`--cert-dir`；换目录要迁移数据，否则 Pod/证书状态丢失 |
| etcd | `controlPlane.etcd.local.dataDir` | `/var/lib/etcd` | 停 etcd → 拷贝数据 → 改配置 → `node repair`；多成员逐个迁移 |

迁移前先备份；不要只改配置而不搬数据。`reset` 会把 PKI 与静态加密配置备份到
`<data-dir>/backup/<timestamp>/`。

## 4. `controlPlane`

```yaml
controlPlane:
  pki: {}
  etcd: {}
  apiServer: {}
  controllerManager: {}
  scheduler: {}
  audit:
    enabled: true
    logMaxAge: 7
    logMaxBackup: 10
    logMaxSize: 100
    policy: ""
  externalEndpoints: []
  certSANs: []
```

### 4.1 `controlPlane.pki`

| 字段 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `keyAlgorithm` | string | `rsa2048` | `rsa2048` 或 `ecdsa-p256` |
| `caValidity` | duration | `87600h`（10 年） | CA 有效期 |
| `certValidity` | duration | `8760h`（1 年） | 叶子证书有效期 |
| `externalCA.certFile` | string | 空 | kubernetes-ca 外部 CA 证书路径（可含中间 CA + 根链） |
| `externalCA.keyFile` | string | 空 | kubernetes-ca 外部 CA 私钥路径 |
| `externalFrontProxyCA.certFile` | string | 空 | front-proxy-ca 外部 CA 证书路径 |
| `externalFrontProxyCA.keyFile` | string | 空 | front-proxy-ca 外部 CA 私钥路径 |
| `externalEtcdCA.certFile` | string | 空 | 本地 etcd 的 etcd-ca 外部 CA 证书路径 |
| `externalEtcdCA.keyFile` | string | 空 | 本地 etcd 的 etcd-ca 外部 CA 私钥路径 |

```yaml
pki:
  keyAlgorithm: rsa2048
  caValidity: 87600h
  certValidity: 8760h
  # 三个信任域可分别导入企业 CA；建议使用三套独立中间 CA，不要复用同一把根。
  # externalCA:
  #   certFile: /usr/local/kubemono/pki/kubernetes-ca-chain.crt
  #   keyFile: /usr/local/kubemono/pki/kubernetes-ca.key
  # externalFrontProxyCA:
  #   certFile: /usr/local/kubemono/pki/front-proxy-ca-chain.crt
  #   keyFile: /usr/local/kubemono/pki/front-proxy-ca.key
  # externalEtcdCA:
  #   certFile: /usr/local/kubemono/pki/etcd-ca-chain.crt
  #   keyFile: /usr/local/kubemono/pki/etcd-ca.key
```

注意：

- duration 支持 `h`、`d`、`w`（例如 `365d`、`52w`）；
- `certValidity` 必须大于 0 且不能超过 `caValidity`；
- 三个 `external*CA` 的 `certFile` 与 `keyFile` 必须成对出现；
- `certFile` 可以包含“签发 CA + 中间 CA + 根 CA”的证书链；kubemono 会把整条链写入 `ca.crt`，签名使用第一张；
- `externalEtcdCA` 只对本地 etcd 生效；`etcd.external` 任一字段非空时 etcd 由用户管理，使用 `controlPlane.etcd.external.caFile/certFile/keyFile`；
- 外部 CA 文件需要在执行 `certs` phase 的 master 上可读；加入 master 时证书包会分发 CA，但 `external*CA` 校验仍会读取配置路径；
- 导入外部 CA 只能在集群创建时做；已有 CA 的 TTL 不能修改；
- 当前不支持 CA 轮换；`cert renew` 只重签叶子证书，CA 指纹必须保持不变；
- `keyAlgorithm`、`caValidity`、三个 `external*CA` 只能在集群创建时设置；`certValidity` 影响后续重签与续期。

> Secret 静态加密固定开启；审计配置见 4.4。不提供 `controlPlane.security` 配置项。

### 4.2 `controlPlane.etcd`

| 字段 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `local.dataDir` | string | `/var/lib/etcd` | 本地 etcd 数据目录 |
| `local.extraArgs` | map[string]string | `{}` | etcd CLI flag 透传 |
| `external.endpoints` | []string | `[]` | external etcd endpoint，必须是 URL |
| `external.caFile` | string | 空 | external etcd CA |
| `external.certFile` | string | 空 | external etcd 客户端证书 |
| `external.keyFile` | string | 空 | external etcd 客户端私钥 |

```yaml
etcd:
  local:
    dataDir: /var/lib/etcd
    extraArgs: {}
  # external:                        # 任一字段非空即切换为外部 etcd
  #   endpoints: ["https://10.0.0.31:2379"]
  #   caFile: /usr/local/kubemono/etcd-ca.crt
  #   certFile: /usr/local/kubemono/etcd-client.crt
  #   keyFile: /usr/local/kubemono/etcd-client.key
```

注意：

- 没有 `type`：`external` 任一字段非空即使用外部 etcd，否则使用本地 etcd；
- local 与 external 同时配置时 external 优先，`local` 会被忽略并给出 warning；
- 外部 etcd 必须提供 endpoints 和三个文件，且 endpoint 必须以 `http://` 或 `https://` 开头；
- external etcd 由用户部署和升级，kubemono 只做启动前健康检查；
- `local.dataDir` 修改需要迁移已有 etcd 数据；preflight、host-init、`reset` 都会使用这个路径；
- `/etc/kubernetes/pki/etcd` 只在本机运行本地 etcd 时创建；纯 worker 与 external etcd 不创建，
  如果该目录为空，会在下次 `install`/`node repair` 成功后自动删除（非空目录保留）；
- `etcd.local.extraArgs` 会重启 etcd，同名 flag 会覆盖 kubemono 默认值；
- 外部 etcd 由用户部署、升级、备份与监控；kubemono 只读取 endpoints/证书并在启动前做健康检查；
- endpoints 会原样进入 apiserver 的 `--etcd-servers`（多个用逗号连接），必须是 `http://` 或 `https://` URL；
- `caFile` 用于校验 etcd 服务端证书，`certFile`/`keyFile` 是 apiserver 的客户端证书；三者都必填；
- 外部 etcd 模式下 `controlPlane.pki.externalEtcdCA` 不生效（它只用于本地 etcd）；
- `local.dataDir` 的迁移：停 etcd → 拷贝数据 → 改配置 → `node repair` → 验证成员健康。

### 4.3 控制面组件参数

```yaml
apiServer:
  extraArgs:
    profiling: "false"
controllerManager:
  extraArgs: {}
scheduler:
  extraArgs: {}
```

| 字段 | 说明 |
|---|---|
| `controlPlane.apiServer.extraArgs` | apiserver CLI flag |
| `controlPlane.controllerManager.extraArgs` | controller-manager CLI flag |
| `controlPlane.scheduler.extraArgs` | scheduler CLI flag |

注意：

- 只写 flag 名（不带 `--`），value 不能含空白；
- 同名 flag 覆盖 kubemono 生成的默认值；
- 审计相关 flag 是例外：请使用 `controlPlane.audit`，不要写在 `extraArgs`；
- `--config`、`--kubeconfig`、`--cert-dir`、`--data-dir`、`--root-dir`、运行时 endpoint
  等由 kubemono 管理的 flag 会被校验拒绝（见 9.1 的保留清单）；
- 修改后需要重启对应组件（仅 master）。

### 4.4 `controlPlane.audit`

```yaml
controlPlane:
  audit:
    enabled: true          # 默认 true
    logMaxAge: 14          # 天；默认 7
    logMaxBackup: 20       # 默认 10
    logMaxSize: 200        # MiB；默认 100
    policy: |              # 可选；为空使用内置默认策略
      apiVersion: audit.k8s.io/v1
      kind: Policy
      rules:
        - level: Metadata
```

| 字段 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `enabled` | bool | `true` | 是否启用 apiserver 审计 |
| `logMaxAge` | int | `7` | 审计日志保留天数，`>= 0` |
| `logMaxBackup` | int | `10` | 保留的审计日志备份数，`>= 0` |
| `logMaxSize` | int | `100` | 单个审计日志文件最大 MiB，`> 0` |
| `policy` | string | `""` | 完整 `audit.k8s.io/v1` Policy YAML；为空使用内置策略 |

注意：

- `policy` 里的 `apiVersion` / `kind` 是 Kubernetes 审计对象自己的格式，不是 cluster.yaml 信封；
- 内置策略：准入/RBAC 写操作记 `Request`，Secret/ServiceAccount/CSR 读取记 `Metadata`，
  其余请求记 `Metadata`；策略文件写到 `/etc/kubernetes/audit/policy.yaml`；
- 审计日志固定写 `/var/log/kubernetes/audit.log`，不提供自定义路径；
- `enabled: false` 时不生成 `--audit-*` flag 和策略文件；已经存在的审计日志不会自动删除；
- 策略或参数变更后需要重启 apiserver（`node repair` 会按文件变更触发重启）；
- 审计相关 flag 不允许写在 `controlPlane.apiServer.extraArgs`，请使用本段配置，避免两个来源；
- `config validate` 会检查策略 YAML、`apiVersion`/`kind`、至少一条规则和 `level` 合法性。

### 4.5 `externalEndpoints` 与 `certSANs`

| 字段 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `externalEndpoints` | []string | `[]` | 集群稳定外部入口（VIP/DNS）列表，可带端口；设置后禁用 worker 本地 haproxy |
| `certSANs` | []string | `[]` | 追加到 apiserver 证书的 DNS/IP SAN |

```yaml
externalEndpoints:
  - "k8s.example.com:6443"
  - "10.0.0.100:6443"
certSANs:
  - "api.example.com"
  - "10.0.0.200"
```

注意：

- 设置 `externalEndpoints` 表示集群已经有稳定的外部入口（VIP/DNS），worker 直接使用它，
  **不部署 haproxy**；
- kubelet 只使用列表中的**第一个** endpoint；多 master 下每个 endpoint 都必须是
  VIP/DNS，不能是某台 master 的地址，否则该 master 故障后 kubelet 不会切换；
- externalEndpoints 会进入 apiserver 证书 SAN，并用于 worker/节点连接；
- 只支持 IPv4；不带端口时按 6443 处理并给出 warning；写成某台 master 的地址会 warning
  （它必须是 VIP/DNS，否则该 master 故障后 kubelet 不会切换）；
- endpoint 必须是 `host[:port]` 或 `https://host[:port]`：带路径/查询串、非法端口、
  `http://`、IPv6 都会在 `config validate` 阶段被拒绝；
- certSANs 只接受 IPv4 或 DNS 名：带端口/路径、通配符、空白、IPv6 会被拒绝；
- 修改这两个字段后需要 master 重签证书（`certs` phase 会按新 SAN 重签并重启 apiserver）。

**apiserver 证书 SAN 的范围**：每台 master 的 apiserver 证书只包含

- 本节点身份：该节点的 `cluster.hosts[].name`、`nodeName` 与全部地址；
- 公共名字：`kubernetes` / `kubernetes.default` / `kubernetes.default.svc` /
  `kubernetes.default.svc.<dnsDomain>`、Service IP、`127.0.0.1`、`localhost`；
- 本配置里的 `externalEndpoints` 与 `certSANs`。

**不会**包含其它 master 的名字或地址——证书只应断言持有者实际能服务的端点。
由此带来两点：

- 如果需要用**另一台 master 的 hostname** 访问（DNS 轮询、SNI 透传的代理、
  NAT/端口映射），必须把这些名字/IP 显式写进 `certSANs`，否则 TLS 校验失败；
  VIP/DNS/LB 这类共享入口写 `externalEndpoints` 或 `certSANs` 即可，它们会进入
  每台 master 的证书。
- 新增或删除 master 不改变存量 master 的 SAN，`node repair` 不会把它们的
  apiserver 证书判成漂移，因此扩容不需要全量重签、重启所有 apiserver。

## 5. `network`

```yaml
network:
  plugin: kube-router
  podCIDR: 10.244.0.0/16
  serviceCIDR: 10.96.0.0/12
  clusterDNS: 10.96.0.10
  dnsDomain: cluster.local
  nodeCIDRMaskSize: 24
  kubeRouter: {}
  kubeProxy: {}
```

| 字段 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `plugin` | string | `kube-router` | `kube-router` 或 `none` |
| `podCIDR` | string | `10.244.0.0/16` | Pod CIDR，不可变 |
| `serviceCIDR` | string | `10.96.0.0/12` | Service CIDR，不可变 |
| `clusterDNS` | string | 由 serviceCIDR 第 10 个地址推导，默认 `10.96.0.10` | DNS Service IP，必须在 serviceCIDR 内 |
| `dnsDomain` | string | `cluster.local` | 集群 DNS 域名，必须是 DNS-1123 子域（小写字母/数字/`-`/`.`） |
| `nodeCIDRMaskSize` | int | `24` | 每节点 Pod 子网前缀长度，必须大于 Pod CIDR 前缀且不超过 32 |
| `kubeRouter` | object | | kube-router（CNI）配置 |
| `kubeProxy` | object | | kube-proxy（Service 代理）配置 |

注意：

- `podCIDR` 与 `serviceCIDR` 不能重叠；
- 节点地址（含 matchAddresses）不能落在 Pod/Service CIDR；
- `nodeCIDRMaskSize` 必须大于 Pod CIDR 前缀长度且不超过 32（IPv4 上限）；
- `podCIDR` / `serviceCIDR` / `clusterDNS` 当前只支持 IPv4；IPv6 配置会被 validation 拒绝；
- `mtu: 0` 在 `install`/`node repair` 中按本机目标网卡 MTU 自动扣除 overlay 开销；
- 这些网络字段只能在新建集群时设置；
- `plugin=none` 表示用户自行部署 CNI：kubemono 不写 `/etc/cni/net.d`，也不创建
  kube-router；Service 代理默认仍由 kube-proxy 承担（见 5.2），使用自带
  kube-proxy replacement 的 CNI 时再关闭它。写法与接入步骤见 5.3。

### 5.1 `network.kubeRouter`

| 字段 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `runRouter` | bool | `true` | 运行 kube-router 路由 |
| `runFirewall` | bool | `true` | 运行 kube-router 网络策略/防火墙 |
| `enableOverlay` | bool | `true` | 启用 overlay |
| `overlayType` | string | `subnet` | `subnet` / `full`（上游 kube-router 只支持这两种） |
| `clusterAsn` | int | `64512` | BGP Cluster ASN，0–4294967295 |
| `mtu` | int | `0` | overlay MTU；0 表示自动 |
| `extraArgs` | map[string]string | `{}` | kube-router CLI flag 透传 |

```yaml
kubeRouter:
  runRouter: true
  runFirewall: true
  enableOverlay: true
  overlayType: subnet
  clusterAsn: 64512
  mtu: 0
  extraArgs: {}
```

注意：

- kube-router 只做 CNI/路由/网络策略：`--run-service-proxy` 固定为 `false`，
  Service 代理由 kube-proxy 承担；不能通过 `extraArgs` 把它改成 `true`（校验会拒绝）；
- `enableOverlay=false` 时要求底层网络能路由 Pod CIDR；
- 上游 kube-router v2.12 没有 `--bgp-peers`：需要 BGP peer 时用
  `extraArgs.peer-router-ips` 与 `extraArgs.peer-router-asns`。配置里已没有
  `bgpPeers` 字段（旧写法会被严格解码拒绝）；`overlay-type=vxlan` 同样不受支持；
- `enable-cni`、`run-service-proxy`、`hostname-override`、`kubeconfig` 是保留 flag，
  `extraArgs` 覆盖会在 `config validate` 阶段被拒绝；
- 这些字段修改后重跑 `node repair` 生效；涉及 overlay/MTU 的改动建议重建节点。

### 5.2 `network.kubeProxy`

Service 代理统一由 kubemono 部署的 kube-proxy DaemonSet 承担（hostNetwork，
使用自己的 ServiceAccount token 访问 apiserver）。

| 字段 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `enabled` | bool | `true` | 是否部署 kube-proxy；自带 replacement 的 CNI 置为 `false` |
| `mode` | string | `ipvs` | `ipvs` / `iptables` / `nftables` |
| `scheduler` | string | `rr` | IPVS 调度器，仅 `mode=ipvs` 时生效；其他模式下写非默认值会被校验拒绝 |

```yaml
kubeProxy:
  enabled: true
  mode: ipvs
  scheduler: rr
```

注意：

- 默认 `ipvs`；`iptables` 仅作为兼容选项，`nftables` 是上游的新实现；
- 上游从 Kubernetes 1.35 起已把 kube-proxy 的 `ipvs` 模式标记为 deprecated
  （1.37 仍可用），长期建议切到 `nftables`（内核满足 5.13+ 时）；
- `nftables` 要求每个节点内核 **>= 5.13**：`config validate` 会提示，`preflight`
  会按本机内核严格校验（不满足时安装被阻止）；
- kube-proxy 在模式切换时会自行清理另一种模式的规则；
- 修改后重跑 master 上的 `node repair`；DaemonSet 会滚动更新，节点逐个切换。

### 5.3 `plugin: none`（第三方 CNI）

`plugin` 只有 `kube-router` 和 `none` 两个值；kubemono 不是 CNI 插件框架，第三方 CNI
通过 `none` 接入，不需要也不能写 `plugin: calico` 之类的值。

```yaml
network:
  plugin: none
  podCIDR: 10.244.0.0/16        # 必须与第三方 CNI 的实际 Pod 网段一致
  serviceCIDR: 10.96.0.0/12     # 必须与外部 Service 代理一致
  clusterDNS: 10.96.0.10
  dnsDomain: cluster.local
  nodeCIDRMaskSize: 24
```

`plugin=none` 时：

- `network.kubeRouter` 整段不生效；
- kubemono 不写 CNI 配置、不创建 kube-router DaemonSet/RBAC/ConfigMap；
- kubemono 仍会安装 CNI 插件二进制（与运行时模式无关）、分配 Node PodCIDR、
  渲染 kubelet 配置；kube-proxy 仍默认部署，使用 Cilium/Calico 等 replacement
  时设置 `network.kubeProxy.enabled: false`；
- 节点在第三方 CNI 就绪前会 `NotReady`，CoreDNS 等 Pod 可能 Pending/ContainerCreating；
- 第三方 CNI 的部署、升级、卸载、防火墙端口由用户负责；`kubemono reset`
  不会清理它。

接入步骤：

1. 用 `plugin: none` 新建集群：`init/join` 会成功，但节点先 `NotReady`，CoreDNS 等 Pod
   可能一直 `ContainerCreating`，直到 CNI 就绪；
2. 按 CNI 官方文档部署，并把 Pod CIDR、Service CIDR 分别对齐 `network.podCIDR`、
   `network.serviceCIDR`（Service 代理的 `clusterCIDR` 也应取 `podCIDR`）；
3. 决定 Service 代理：CNI 只做 Pod 网络时保留 `network.kubeProxy.enabled: true`；
   CNI 自带 kube-proxy replacement（Cilium/Calico eBPF 等）时设为 `false`；
4. 等节点 `Ready`，用测试 Pod 验证跨节点 Pod 互通、DNS、ClusterIP/NodePort。

接入要点：

- `custom` 运行时还要确认 CRI 的 CNI `bin_dir` 指向 `/opt/cni/bin`（kubemono 会把插件装到
  这里；若 CRI 配了别的目录，需要自行复制/软链）；
- kubemono 不创建/升级/删除第三方 CNI 的 DaemonSet、CRD、RBAC、ConfigMap，也不替它开防火墙；
- `kubemono reset` 只清理 kubemono 自己的网络文件，不会删除第三方 CNI。

---

## 6. `addons`

### 6.1 `addons.coredns`

| 字段 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `enabled` | bool | `true` | 安装 CoreDNS |
| `replicas` | int | `2` | 副本数；不可为负 |
| `upstream` | []string | `["/etc/resolv.conf"]`（启用且为空时） | 上游 DNS 解析器 |
| `hosts` | []string | `[]` | `IP host [alias]` 形式的 hosts 条目 |
| `logFormat` | string | `text` | 进程日志格式：`text` 或 `json` |
| `quiet` | bool | `false` | 关闭启动时的版本/初始化输出 |

CoreDNS 的配置主体是 Corefile；受管区块之外的用户扩展在 addon phase 重跑时保留。
这里不开放通用 `extraArgs`：`conf`、`version`、`plugins` 等 flag 会破坏 Corefile 或让容器直接退出。

```yaml
coredns:
  enabled: true
  replicas: 2
  upstream:
    - /etc/resolv.conf
  hosts:
    - "10.0.0.50 registry.local"
  logFormat: text
  quiet: false
```

注意：

- `enabled=true` 时 `network.clusterDNS` 必须非空；
- `replicas=0` 合法但会导致没有 DNS Pod；
- `upstream` 可以是文件路径或解析器地址；使用 `/etc/resolv.conf` 时由节点解析；
- `hosts` 条目格式为 `IP host [alias...]`；


### 6.2 kube-proxy

kube-proxy 是 `network` 的一部分，不放在 `addons` 下：

- 配置入口是 `network.kubeProxy`（见 5.2），由 kubemono 渲染
  DaemonSet/ConfigMap/ServiceAccount/ClusterRoleBinding 并随 `node repair` 收敛；
- `network.plugin=kube-router` 时 kube-router 只做 CNI，不自动接管 Service；
- `network.plugin=none` 时默认仍部署 kube-proxy；使用 Cilium/Calico 的
  kube-proxy replacement 时把 `network.kubeProxy.enabled` 设为 `false`；
- 不接受 `addons.kubeProxy` 字段（严格字段校验会拒绝）。

---

## 7. `images`

```yaml
images:
  components:
    coredns: registry.example.com/kubemono/coredns/coredns:v1.14.6
  componentVersions: {}
```

| 字段 | 类型 | 默认值 | 说明 |
|---|---|---|---|
| `components` | map[string]string | `{}` | 按组件覆盖**完整**镜像引用 |
| `componentVersions` | map[string]string | `{}` | 非 k8s 组件版本覆盖 |

### 7.1 按组件改镜像

镜像没有全局仓库前缀。BOM 为每个需要镜像的组件写一条完整引用；
cluster.yaml 用同名的 `images.components.<component>` 覆盖它：

```yaml
images:
  components:
    pause: registry.example.com/kubemono/pause:3.10.2
    kube-proxy: registry.example.com/kubemono/kube-proxy:v1.37.1
    kube-router: registry.example.com/kubemono/kube-router:v2.12.0
    coredns: registry.example.com/kubemono/coredns/coredns:v1.14.6
    haproxy: registry.example.com/kubemono/haproxy:3.4
```

规则：

- key 必须是 BOM 中声明了 `image` 的组件；没有镜像的组件（如 containerd、
  runc、etcd、kubelet）不允许写；
- value 必须写全（含 registry host 与 tag/digest），不依赖隐式 Docker Hub；
- 当前 BOM 中可写 `images.components` 的组件：`pause`、`kube-proxy`、`kube-router`、
  `coredns`、`haproxy`（以 `bom.yaml` 为准，见 [`bom.md`](bom.md)）；
- 未写的组件继续使用 BOM 的完整引用；
- 覆盖后，出包与节点侧都使用同一个引用；改镜像后必须重新出包/同步镜像，
  否则节点会拉不到镜像。

#### 私有仓库的认证

镜像引用只负责“去哪里拉”，认证仍按 registry host 配在 containerd：

```yaml
node:
  runtime:
    containerd:
      registries:
        configs:
          registry.example.com:
            caFile: /usr/local/kubemono/registry-ca.crt
            username: robot
            password: "******"
```

- 同一个 host 下的多个镜像共用一份认证，不需要在每个 `image` 里重复写账号；
- 不要把用户名/密码写进 `images.components.*`；
- 认证文件以 0600 落盘；cluster.yaml 本身也应按 0600 保护；
- 需要多个私有仓库时，为每个 host 各写一条 `configs`。

#### pull-through mirror（不改镜像引用）

```yaml
node:
  runtime:
    containerd:
      registries:
        mirrors:
          registry.k8s.io: ["https://mirror.example.com"]
```

- mirror 不改变镜像引用，只改变 containerd 的拉取路径；
- 与 `images.components` 是两套机制：前者改“引用”，后者改“拉取路径”。

### 7.2 组件版本覆盖

```yaml
images:
  componentVersions:
    coredns: v1.14.7
```

注意：

- 只能覆盖 BOM 中存在的组件；
- k8s 组件用顶层 `kubernetesVersion` 整组固定，不在 `componentVersions` 里逐个写；
- 不能写 `kubemono` 自身；
- 任意版本都允许，不检查最低版本/兼容范围；
- 与默认版本不同时只打印 warning；kubemono 只保证默认版本，兼容性由操作者负责；
- 非默认版本需要先准备对应离线物料；
- 非 k8s 组件版本变更需要先准备物料并重建节点；
- 当前 BOM 中可用 `componentVersions` 覆盖的组件：`containerd`、`runc`、`cni-plugins`、
  `pause`、`etcd`、`kube-router`、`coredns`、`haproxy`；k8s 组件与 `kubemono` 自身会被拒绝；
- 版本、下载地址（`source`）、镜像与架构的完整规则，以及 `bundle pack --bom` 的边界，
  见 [`bom.md`](bom.md)。

### 7.3 离线物料一致性

`images.components.*` 会改变节点期望的镜像引用。物料归档必须按同一组引用生成；
改镜像后需要重新打包/推送，否则节点会拉不到镜像。`kubemono bundle pack`
（`-f cluster.yaml`）与节点命令使用同一份生效 BOM。

---

## 8. 可自定义项一览

按段汇总「用户可写、且会产生实际效果」的字段。详细规则见各节；表中的 repair 指
`kubemono node repair`，master 上会额外收敛集群级对象。

### 8.1 集群与节点

| 段 | 可自定义项 | 作用 / 典型用途 | 生效方式 |
|---|---|---|---|
| 顶层 | `kubernetesVersion` | 整组固定 Kubernetes 版本 | 仅新建集群；物料需按新 release 准备 |
| `cluster.hosts[]` | `name` / `nodeName` / `address` / `matchAddresses` / `roles` | 节点身份、地址与角色 | 新增/替换节点走 `join`；改名用 `repair --rebind-identity`；地址变化用 `repair` |
| `cluster.hosts[]` | `labels` / `taints` | Node 标签与污点 | `repair`（worker 改 taints 需要 admin 凭据） |
| `cluster.hooks` | `preHostInit` / `postRuntime` / `postInstall` | 三个时点的 root shell 钩子 | 下次 `init`/`join`/`repair`；带 `--only` 时不跑 `postInstall` |
| `node.host` | `kernelModules` / `sysctls` | 主机基线（整份替换） | `repair` |
| `node.runtime.containerd` | `root` / `systemdCgroup` / `registries` / `configPatch` | 托管 containerd | `repair`；`root` 需迁移数据 |
| `node.runtime.custom` | `criSocket` / `cgroupDriver` / `serviceName` | 自备 CRI | `repair`；与 containerd 互切需重建 |
| `node.kubelet` | `rootDir` / `configPatch` / `extraArgs` | kubelet | `repair`；`rootDir` 需迁移数据 |

### 8.2 控制面

| 段 | 可自定义项 | 作用 / 典型用途 | 生效方式 |
|---|---|---|---|
| `controlPlane.pki` | `keyAlgorithm` / `caValidity` / `external*CA` | CA 算法、有效期、导入企业 CA | 仅新建集群 |
| `controlPlane.pki` | `certValidity` | 叶子证书有效期 | `repair`（certs）或 `cert renew` |
| `controlPlane.etcd.local` | `dataDir` / `extraArgs` | 本地 etcd | `repair`；`dataDir` 需迁移数据 |
| `controlPlane.etcd.external` | `endpoints` / `caFile` / `certFile` / `keyFile` | 外部 etcd | `repair`；local ↔ external 需重建 |
| `controlPlane.apiServer` / `controllerManager` / `scheduler` | `extraArgs` | 控制面组件 flag | `repair`（master） |
| `controlPlane` | `externalEndpoints` / `certSANs` | 稳定入口与额外证书 SAN | `repair`（master，重签 + 重启） |

### 8.3 网络

| 段 | 可自定义项 | 作用 | 生效方式 |
|---|---|---|---|
| `network` | `plugin` | CNI 实现（kube-router/none） | 仅新建集群 |
| `network` | `podCIDR` / `serviceCIDR` | Pod/Service 网段 | 仅新建集群 |
| `network` | `clusterDNS` / `dnsDomain` / `nodeCIDRMaskSize` | DNS 与节点子网 | 仅新建集群 |
| `network.kubeRouter` | `runRouter` / `runFirewall` / `enableOverlay` / `overlayType` / `clusterAsn` / `mtu` / `extraArgs` | 内置 CNI/路由/网络策略 | `repair`；overlay/MTU 改动建议重建节点 |
| `network.kubeProxy` | `enabled` / `mode` / `scheduler` | Service 代理 | `repair`（master，DaemonSet 滚动） |

### 8.4 附加组件与镜像

| 段 | 可自定义项 | 作用 | 生效方式 |
|---|---|---|---|
| `addons.coredns` | `enabled` / `replicas` / `upstream` / `hosts` / `logFormat` / `quiet` | 内置 DNS | `repair`（master） |
| `images.components` | 每个镜像组件的完整引用 | 私有仓库/镜像重写 | 重新出包或同步镜像 + `repair` |
| `images.componentVersions` | 非 k8s 组件版本 | 组件版本覆盖 | 准备对应物料 + `repair` |
| `bom.yaml`（不是 cluster.yaml） | 组件版本 / `source` URL / 镜像模板 / 架构 | 自定义发行版与物料来源 | 只影响 `bundle pack`/`list`，见 [`bom.md`](bom.md) |

### 8.5 不可自定义（固定项）

- Secret 静态加密固定开启，没有 `controlPlane.encryption`/`controlPlane.security` 配置项：
  密钥只在本机生成并以 0600 保存；关闭或轮换会让已有加密 Secret 不可读，因此不提供
  在线开关；
- apiserver 审计默认开启，通过 `controlPlane.audit` 配置；策略为空时使用内置安全策略；
- apiserver 的 `authorization-mode=Node,RBAC`、`NodeRestriction`、bootstrap token auth、
  aggregator routing、`allow-privileged` 是安全/启动基线，不开放配置；
- kubelet 的 `readOnlyPort=0`、匿名认证关闭、webhook 认证/授权是安全基线，不开放配置；
- 审计日志路径固定为 `/var/log/kubernetes/audit.log`；策略文件固定为
  `/etc/kubernetes/audit/policy.yaml`，因为两者与静态 Pod 的读写挂载绑定；
- 没有可配置的集群名：集群身份是安装时生成的 `clusterID`；
- `addons` 下只有 CoreDNS，没有 `addons.kubeProxy` 等字段；
- k8s 组件不能逐组件固定版本，只能整组跟随 `kubernetesVersion`；
- kubemono 自身版本属于 release，不能被覆盖。

版本优先级：`cluster.yaml` 的 `kubernetesVersion` / `images.*` 覆盖内置 BOM；三者合并后的
生效 BOM 决定组件版本、镜像引用与 `release`。完整规则与打包边界见 [`bom.md`](bom.md)。

---

## 9. 自定义参数总览

### 9.1 `extraArgs`

适用于：

- `controlPlane.apiServer.extraArgs`
- `controlPlane.controllerManager.extraArgs`
- `controlPlane.scheduler.extraArgs`
- `controlPlane.etcd.local.extraArgs`
- `node.kubelet.extraArgs`
- `network.kubeRouter.extraArgs`

规则：

- key 是 flag 名，不带前导 `--`；
- value 是字符串；不能包含空白；
- 同名 flag 覆盖 kubemono 默认值；
- 但决定集群身份/信任根/数据目录/端口的 flag 是**保留项**，覆盖会在 `config validate`
  阶段被拒绝（例如 apiserver 的 `authorization-mode`、etcd 的 `data-dir`、kubelet 的
  `kubeconfig`、kube-router 的 `enable-cni`/`hostname-override`）；
- 未知 flag 由组件启动时拒绝；
- 不是所有配置字段都有对应 CLI flag；先查组件文档再写 `extraArgs`。

### 9.2 `configPatch`

适用于：

- `node.kubelet.configPatch`：合并进 kubelet YAML；
- `node.runtime.containerd.configPatch`：内联 TOML 合并。

规则：

- 嵌套 map 递归合并，标量覆盖；
- 不做 schema 校验；
- `kubelet.configPatch` 的 `readOnlyPort`、`authentication`、`authorization`、
  `staticPodPath`、`containerRuntimeEndpoint`、`cgroupDriver`、`clusterDomain`
  是保留键，校验会拒绝（它们决定安全基线或 kubemono 的接线）；
- 覆盖 kubemono 管理的路径（kubeconfig、集群 CIDR、健康检查端口等）可能破坏安装；
- 修改后先 `kubemono config validate`，再在单节点验证。

### 9.3 `cluster.hooks`

见 2.2；钩子不是“配置补丁”，是 root shell 命令，失败会中止安装，且不会自动回滚。

---

## 10. 校验与常见坑

### 10.1 严格字段

- 未知字段会报错，不会静默忽略；
- 所有 duration 使用字符串，支持 `h`/`d`/`w`；
- 数值型 sysctls/extraArgs 建议加引号，避免 YAML 类型转换。

### 10.2 常见错误

1. **把 k8s 组件写进 `componentVersions`**：应使用顶层 `kubernetesVersion`。
2. **把没有镜像的组件写进 `images.components`**：containerd/runc/etcd/kubelet 没有镜像，写该组件会被拒绝。
3. **把 `clusterDNS` 写到 serviceCIDR 外**：校验失败或 DNS 不通。
4. **`nodeName: hostname` 且多 master**：etcd/证书身份依赖各机器 hostname，跨节点推导困难；显式写 `nodeName`。
5. **`plugin: none` 后以为 kubemono 会装 CNI**：kubemono 不写 `/etc/cni/net.d`；节点会一直
   `NotReady`，直到第三方 CNI 就绪。CNI 的 Pod/Service 网段必须与 `network` 一致。
6. **配置了 `runtime.custom` 但 CRI 的 CNI 目录没对齐**：`runtime` phase
   用配置的 CRI socket 做就绪探测；CNI 插件由 kubemono 装到 `/opt/cni/bin`，但自定义 CRI 可能
   配置了别的 `bin_dir`，需要自行复制/软链。
7. **切换到 `nftables` 但内核太旧**：`network.kubeProxy.mode=nftables` 要求节点内核
    >= 5.13；`config validate` 只提示，`preflight` 会阻止安装。
8. **想原地切换 `network.plugin`、containerd/custom 或 etcd local/external**：只能新建集群并迁移工作负载。
9. **改了镜像引用但没重新生成物料**：节点仍按旧引用拉取，导致 ImagePullBackOff。
10. **`configPatch` 覆盖 kubeconfig/clusterCIDR/healthz 端口**：kubemono 的其他组件仍按默认假设工作，可能直接启动失败。
11. **用 `node.kubelet.extraArgs.root-dir` 改数据目录**：只会改 flag，`--config`/`--cert-dir`/kubeconfig 仍在原路径；请用 `node.kubelet.rootDir`。

---

## 11. 完整骨架

下面是「全字段展开」形态（相当于 `kubemono config default` 的内容再补上少量可选项）；
实际书写只需 §1 的最小配置，其余段按需增删，未写的字段一律取默认值。

```yaml
kubernetesVersion: v1.37.4

cluster:
  hosts:
    - name: m1
      nodeName: address
      address: 10.0.0.11
      matchAddresses: []
      roles: [master, worker]
      labels: {}
      taints: []
    - name: w1
      nodeName: hostname
      address: 10.0.0.21
      roles: [worker]
  hooks:
    preHostInit: []
    postRuntime: []
    postInstall: []

node:
  host:
    kernelModules: [overlay, br_netfilter, nf_conntrack, ip_vs, ip_vs_rr, ip_vs_wrr, ip_vs_sh, ipip]
    sysctls:
      net.ipv4.ip_forward: "1"
      net.bridge.bridge-nf-call-iptables: "1"
      fs.inotify.max_user_instances: "8192"
      fs.inotify.max_user_watches: "524288"
  runtime:
    containerd:
      root: /var/lib/containerd
      systemdCgroup: true
      registries:
        mirrors: {}
        configs: {}
      configPatch: ""
    custom: {}
  kubelet:
    rootDir: /var/lib/kubelet
    configPatch: {}
    extraArgs: {}

controlPlane:
  pki:
    keyAlgorithm: rsa2048
    caValidity: 87600h
    certValidity: 8760h
    externalCA: {}
    externalFrontProxyCA: {}
    externalEtcdCA: {}
  etcd:
    local:
      dataDir: /var/lib/etcd
      extraArgs: {}
    external: {}
  apiServer: {}
  controllerManager: {}
  scheduler: {}
  audit:
    enabled: true
    logMaxAge: 7
    logMaxBackup: 10
    logMaxSize: 100
    policy: ""
  externalEndpoints: []
  certSANs: []

network:
  plugin: kube-router
  podCIDR: 10.244.0.0/16
  serviceCIDR: 10.96.0.0/12
  clusterDNS: 10.96.0.10
  dnsDomain: cluster.local
  nodeCIDRMaskSize: 24
  kubeRouter:
    runRouter: true
    runFirewall: true
    enableOverlay: true
    overlayType: subnet
    clusterAsn: 64512
    mtu: 0
    extraArgs: {}
  kubeProxy:
    enabled: true
    mode: ipvs
    scheduler: rr

addons:
  coredns:
    enabled: true
    replicas: 2
    upstream: [/etc/resolv.conf]
    hosts: []
    logFormat: text
    quiet: false

images:
  components: {}
  componentVersions: {}
```
