# kubemono 命令与参数详解

本文是 `cluster.yaml` 之外的另一半：**每条命令做什么、什么时候用、参数怎么填、会改什么**。
配合 [`cluster-config.md`](cluster-config.md) 阅读：那份文档解释配置字段与默认值，这份文档解释操作动作与命令行参数。

- 适用对象：`kubemono` 单二进制（版本由 BOM 的 `release` 决定），无守护进程、无常驻 agent。
- 执行位置：安装/修复/重置/证书命令都在**目标节点本机**执行；`bundle pack` 在构建机执行；
  `config default`、`cert show`、`pki issue` 是离线命令，不需要集群。
- 权限：安装、重置、证书命令需要 **root**；集群级对象需要 master 上的
  `/etc/kubernetes/admin.conf`（或 `--kubeconfig` 指定）。
- 权威来源：命令、flag、默认值以 `kubemono <命令> --help` 与本文为准；配置字段以
  `cluster-config.md` 为准。两者冲突时以代码为准，并请提 issue。

---

## 1. 通用约定

### 1.1 命令形态与帮助

```bash
kubemono <命令> [<子命令>] [flags]     # 日常形态
kubemono --help                        # 顶层命令列表
kubemono help <命令>                   # 等价于 kubemono <命令> --help
kubemono <命令> --help                 # 查看该命令的全部 flag
kubemono completion bash               # 生成补全脚本（bash|zsh|fish|powershell）
```

- 参数顺序无关紧要；flag 支持 `--flag value` 与 `--flag=value`，短 flag 用 `-f cluster.yaml`。
- 字符串列表 flag（`--only`、`--cert`、`--san`、`--organization`）可**重复给出或逗号分隔**。
- 命令组（`node`、`token`、`cert`、`pki`）不带子命令时会打印帮助；`node`/`token`/`cert`/`pki`
  还会返回错误（退出码 1），`config`/`bundle` 只打印帮助（退出码 0）。脚本里请总是写全子命令。

### 1.2 全局 flag 只有 `-v`

kubemono 刻意**没有**一长串全局 flag：只有下面这个 persistent flag 对所有命令生效，
其余共享 flag 都按命令挂载——避免 `kubemono version --wait` 这类调用被静默忽略。

| flag | 默认值 | 作用 |
|---|---|---|
| `-v, --verbose` | false | 输出调试信息（调试行带 `·` 前缀） |

### 1.3 共享 flag

这些 flag 由多个命令共用，语义一致；每个命令实际支持哪些见 §12 矩阵。

| flag | 默认值 | 作用 | 关键细节 |
|---|---|---|---|
| `-f, --config` | `/usr/local/kubemono/cluster.yaml` | 指定集群配置文件 | 显式给出但文件读不到 → 直接报错；省略且默认文件不存在时，各命令行为不同（见各命令「前置条件」） |
| `--data-dir` | `/usr/local/kubemono` | kubemono 自身数据目录 | 只影响 `node.yaml`、配置缓存、`artifacts/`、`state/`、`backup/`、`desired/`；**不**改变 `/etc/kubernetes`、`/var/lib/*`；`/` 会被拒绝 |
| `--node` | 空 | 显式指定本机在配置中的身份键 | 取值为 `cluster.hosts[].name`，不是 `nodeName`；优先级最高（见 §1.4） |
| `--allow-address-mismatch` | false | 允许 `--node` 的地址不在本机网卡上 | 只对 `--node` 的地址校验生效，NAT 场景使用；单独使用没有意义 |
| `-y, --assume-yes` | false | 跳过交互确认 | **只**跳过确认，不改变检查范围、不改变续期/清理范围 |
| `--dry-run` | false | 只报告将要执行的动作 | 深度因命令而异：安装类打印计划，`reset` 打印删除清单，`cert renew` 打印续期表 |
| `--wait` | `30m` | 阻塞操作的最长等待时间 | 目前只挂在 `join`：等待集群/apiserver 可达，只重试瞬时错误 |
| `--kubeconfig` | `/etc/kubernetes/admin.conf` | 集群级命令使用的凭据 | 挂在 `reset`、`node repair` 与 `token create`；`reset` 中**只在 `--leave` 时**允许 |
| `--output` | `table` | 结果输出格式 `table\|json\|yaml` | 仅部分命令支持；`json/yaml` 时进度消息改走 stderr，stdout 只留机器可读正文；`config default` 的取值是 `yaml\|json`（文档格式） |

### 1.4 本机身份如何确定

所有需要「本机是哪台节点」的命令（`init`、`join`、`node repair`、`whoami`、`reset --leave`）
都按同一优先级解析：

1. `--node <name>`（显式指定，最高优先级）；
2. `/usr/local/kubemono/node.yaml`（上次安装写入的身份）；
3. 用本机网卡地址匹配 `cluster.hosts[].address` / `matchAddresses`。

由此得到两个实用结论：

- 节点装好后 `node.yaml` 会「钉住」身份。主机名变了、网卡加了地址，都**不会**自动改成另一台节点；
- 要把已安装节点改名（`cluster.yaml` 里 `hosts[].name` 改了），必须
  `kubemono node repair --node <新名> --rebind-identity`，否则会被拒绝；
- 如果 `node.yaml` 记录的节点已从配置里删除，命令会直接报错（`本机身份文件 ... 声明的节点 ... 不在当前配置中`），
  不会退回网卡匹配；按提示 `reset` 清除本机身份，或用 `--node` 指定正确条目。

`hosts[].nodeName` 决定真正的 Kubernetes NodeName（hostname/address/显式名），与
`--node` 的配置身份不是一回事；详见 `cluster-config.md` 的 `cluster.hosts[]`。

### 1.5 输出、进度与退出码

| 流 | 内容 |
|---|---|
| stdout | 命令正文：表格、JSON/YAML、`token create` 打印的 join 命令、安装计划 |
| stderr | `warning:`、`error:`、以及 `--output json\|yaml` 时的全部进度消息 |

- 退出码：**0 成功，1 失败**（含参数错误、校验失败、用户取消、检查发现异常）。
- 取消操作时返回 `已取消`，退出码 1；确认之前的破坏性动作不会执行。
- 以下命令会因「检查结果」返回非零：
  - `cert check-expiration`：有证书过期/无法解析/与 kubeconfig 引用不一致；
    加 `--fail-on-warn` 后进入告警窗口也返回非零；
  - `bundle verify`：归档或物料库校验不通过；
  - `status --check`：未安装/漂移/待重启/证书过期/Node 未 Ready/required unit 未运行。
- 警告（`warning:`）不影响退出码。

### 1.5.1 并发写锁

同一 `--data-dir` 下的写操作（`init`、`join`、`node repair`、`cert renew`、`reset`、
`reset --leave`）通过 `<data-dir>/state/kubemono.lock` 的 `flock` 互斥。第二个进程会
直接失败，而不是两个进程同时改 unit/证书/desired 快照；进程退出/崩溃时内核自动释放锁。
`--dry-run` 不获取锁；`bundle load`/`bundle pack` 不在这把锁的范围内。
如果看到“另一个 kubemono 操作正在运行”，等前一个命令结束再重试即可，不需要删锁文件。

### 1.5.2 状态、备份与敏感文件

- `backup/<timestamp>/` 只保留最近 10 份（`render`/`reset` 共用裁剪逻辑）；旧备份自动删除。
- `desired/files/` 只镜像非 Secret 文件；`containerd` 的 `hosts.toml` 等含 registry
  凭据的文件不会写入 desired 快照，`status`/`--diff` 只报路径与状态。
- 切换 plugin/registry 后，`desired/files/` 中不再声明的旧文件会被清理。
- 普通 `reset` 会把 PKI 备份到 `backup/<timestamp>/pki` 后保留；**退役/转用节点请用
  `reset --purge`**，否则旧 CA 私钥仍在该目录。

### 1.6 交互确认

`reset` 与 `cert renew` 是破坏性操作，默认要求输入 `y` 确认：

- `-y/--assume-yes` 跳过确认（适合脚本/CI）；
- `--dry-run` 也会跳过确认（反正不写）；
- stdin 关闭或没有输入时读不到确认 → 按「拒绝」处理，返回 `已取消`。
  所以非交互环境执行这两个命令必须显式加 `-y`。

### 1.7 执行前提速查

| 命令 | root | `cluster.yaml` | 集群可达 | 说明 |
|---|---|---|---|---|
| `config default` | 否 | 否 | 否 | 纯模板输出 |
| `config validate` | 否 | 是 | 否 | 只读文件 |
| `bundle pack` | 否 | 否（可选） | 否（下载物料时需要网络） | 构建机执行 |
| `bundle list` / `verify` | 否 | 否（库模式建议有） | 否 | 只读 |
| `bundle load` | 通常需要（写 `/usr/local/kubemono`） | 否（有则做一致性提示） | 否 | 导入物料 |
| `init` | 是 | 是 | 否（但会探测 6443 是否已被占用） | master 角色；不生成 join 凭证 |
| `token create` | 是 | 是 | 是 | 在 master 上签发 join 凭证；需要 admin kubeconfig |
| `join` | 是 | 是 | 是（等待上限 `--wait`） | 需要 `--token` |
| `node repair` | 是 | 是 | master 上的集群级 phase 需要 | 需要 `node.yaml` |
| `cert check-expiration` | 通常需要（读 PKI） | 否 | 否 | 只读 |
| `cert renew` | 是 | 可选 | 否 | 需 CA 私钥（master） |
| `cert show` | 否 | 否 | 否 | 只读 PEM |
| `pki issue` | 否 | 否 | 否 | 离线签发 |
| `reset` | 是 | 可选（`--leave` 需要） | `--leave` 需要 | 本机清理 |
| `whoami` | 否 | 是 | 否 | 只读身份 |
| `version` | 否 | 否 | 否 | 只读 |

---

## 2. 命令总览

| 顶层命令 | 子命令 | 一句话用途 | 会改主机 | 会改集群 |
|---|---|---|---|---|
| `init` | — | 引导新集群（不生成 join 凭证） | 是 | 是 |
| `token` | `create` | 签发 join 凭证并打印 join 命令（默认 worker） | 否 | 是 |
| `join` | — | 以加入者身份安装本节点 | 是 | 是 |
| `node` | `repair` | 幂等重跑已安装节点的 phase（日常变更唯一入口） | 是 | master 上会 |
| `config` | `default` `validate` | 生成模板与严格校验 | 否 | 否 |
| `cert` | `check-expiration` `renew` `show` `timer` | 证书巡检、续期、只读查看、自动续期 timer | `renew`/`timer` 是 | 否 |
| `pki` | `issue` | 用已有 CA 离线签发叶子证书 | 否 | 否 |
| `bundle` | `pack` `list` `load` `verify` | 造包、查看、导入、校验物料 | `load` 是 | 否 |
| `reset` | —（`--leave`） | 本机重置；`--leave` 先退群 | 是 | `--leave` 时是 |
| `status` | — | 只读汇总本机收敛状态 | 否 | 否（可查询 Node Ready） |
| `whoami` | — | 只读打印本机身份 | 否 | 否 |
| `version` | — | 版本与固定组件版本 | 否 | 否 |

「我要做什么」速查：

| 目标 | 命令 |
|---|---|
| 生成一份可用的 `cluster.yaml` | `kubemono config default > cluster.yaml` |
| 安装前检查配置 | `kubemono config validate -f cluster.yaml` |
| 离线造包 / 导入包 | `kubemono bundle pack` / `kubemono bundle load <archive>` |
| 初始化第一台 master | `kubemono init -f cluster.yaml` |
| 签发节点加入凭证 | `kubemono token create`（worker）/ `kubemono token create --role master`（控制面） |
| 加入新节点 | `kubemono join <endpoint> --token <id.secret> --discovery-token-ca-cert-hash sha256:<hex> -f cluster.yaml` |
| 应用 `cluster.yaml` 变更 | `kubemono node repair -f cluster.yaml` |
| 只看某一步要改什么 | `kubemono node repair -f cluster.yaml --diff` |
| 只重跑一个 phase | `kubemono node repair -f cluster.yaml --only <phase>` |
| 查证书到期 | `kubemono cert check-expiration` |
| 续期证书 | `kubemono cert renew -f cluster.yaml` |
| 查看证书内容 | `kubemono cert show /etc/kubernetes/pki/ca.crt` |
| 给外部 etcd 签客户端证书 | `kubemono pki issue --ca ... --ca-key ... --cn ... --profile client` |
| 节点退出集群并清理 | `kubemono reset --leave` |
| 重装节点前彻底清理 | `kubemono reset --purge -y` |
| 排查「本机被当成哪台节点」 | `kubemono whoami` |
| 一眼看收敛/证书/unit/Node 状态 | `kubemono status` |

---

## 3. `kubemono init`

### 3.1 使用场景

- **第一次建立集群**：在第一台 master 上执行，创建控制面、网络与附加组件；
- **灾难恢复重建**：本机残留旧 etcd 数据时用 `--force-init`；已有 apiserver 仍可达时用
  `--allow-existing-cluster` 明确表示要重建。

`init` **不生成任何 join 凭证**；加节点时用 `kubemono token create`。它也不是日常变更入口：
已安装节点改配置应使用 `node repair`。

### 3.2 语法与前置条件

```bash
kubemono init -f cluster.yaml [flags]
```

| 前置条件 | 说明 |
|---|---|
| 角色 | 必须是 `roles` 含 `master` 的主机；纯 worker 会报错 |
| 权限 | root |
| 配置 | `cluster.yaml` 必须存在且通过严格校验（未知字段会被拒绝） |
| 物料 | 运行组件来自 `/usr/local/kubemono/artifacts/<release>`；干净节点应先 `bundle load` |
| 端口 | 6443 未被占用：任一 master 的 `address:6443` 可连通即视为「已有集群」，会拒绝 init |
| etcd 数据 | 本机 `/var/lib/etcd` 有残留数据时拒绝，需 `--force-init` |
| 网络 | 不需要外网；容器镜像由 runtime phase 从物料导入（纯控制面 `[master]` 不跑 runtime） |

### 3.3 参数

| flag | 必填 | 默认值 | 作用 | 说明 |
|---|---|---|---|---|
| `-f, --config` | 否 | `/usr/local/kubemono/cluster.yaml` | 集群配置 | 显式 `-f` 读不到会失败 |
| `--node` | 否 | 空 | 指定本机配置身份 | 最高优先级 |
| `--allow-address-mismatch` | 否 | false | 允许 `--node` 地址不在本机网卡 | NAT 场景 |
| `--dry-run` | 否 | false | 只打印计划 | 不写任何文件 |
| `--data-dir` | 否 | `/usr/local/kubemono` | 数据目录 | 同时决定配置缓存/身份文件目录 |
| `--force-init` | 否 | false | etcd 有残留数据仍执行 | 仅灾难恢复 |
| `--allow-existing-cluster` | 否 | false | 跳过「已有 apiserver 可达」检查 | 重建集群时使用 |

### 3.4 它做了什么

1. 解析本机身份 → 打印计划（`节点/角色/动作/执行顺序`）；
2. 生成不可变的集群身份 `clusterID`；
3. 按角色执行 phase：**在 §5.4 的 repair 表最前面加 `preflight`**（init 的
   `cluster-init` 已包含在该表内）。纯控制面 `[master]` 不安装 kubelet/containerd/CNI，
   不注册 Node；表中的 runtime/kubelet/mark-control-plane 只出现在 worker 行；
4. `cluster-init` 发布 `kube-public/cluster-info` 的静态部分、写入
   `kube-system/kubemono-cluster`；**不注册引导令牌，也不上传证书包**；
5. 成功后写本机身份 `node.yaml`、配置缓存 `cluster.yaml`、`desired/` 快照，并执行
   `postInstall` 钩子（若配置了），最后提示用 `kubemono token create` 签发加入凭证。

### 3.5 初始化之后：签发 join 凭证

`init` 只把集群建起来，不生成任何加入凭证。在任意一台已初始化、有 `admin.conf`
和集群 PKI 的 master 上执行：

```bash
# 只加 worker（默认角色，不碰 certificateKey / 证书包）
kubemono token create

# 加 master / 控制面：必须显式指定 --role master
kubemono token create --role master
```

| 角色 | 生成的物料 | 对集群的改动 | stdout |
|---|---|---|---|
| worker（默认） | 1 个 bootstrap token + cluster-info JWS | 只注册 token Secret | 1 条 worker join 命令 |
| master（`--role master`） | 1 个 bootstrap token + 1 个 certificateKey + cluster-info JWS | 注册 token Secret、轮换 certificateKey、刷新 `kube-system/kubemono-certs` | 1 条 master join 命令 |

参数：

| flag | 默认值 | 作用 |
|---|---|---|
| `--role` | `worker` | 签发角色；master 必须显式写 `--role master` |
| `--ttl` | `2h` | bootstrap token 有效期；硬上限 2h |
| `-f, --config` | `/usr/local/kubemono/cluster.yaml` | 集群配置（决定 join 命令里的 endpoint 与 `-f` 路径） |
| `--kubeconfig` | `/etc/kubernetes/admin.conf` | 集群级凭据；可指向其他 master |
| `--dry-run` | false | 只预演，不创建 token、不刷新证书包 |
| `--data-dir` | `/usr/local/kubemono` | 数据目录（PKI、配置缓存） |

- 命令不落盘；stdout 只有 join 命令，进度与告警在 stderr；
- bootstrap token 默认 2h 有效（硬上限 2h），同角色的多个节点可在有效期内**复用同一条**命令；
- **worker 凭证不会碰 certificateKey**，因此不会让已有的 master 命令失效；
- **master 凭证会轮换 certificateKey**，已签发的 master join 命令立即失效，已有的 worker
  命令不受影响；
- 该命令还可用 `--ttl` 缩短有效期，`--kubeconfig` 指定其他 master 的凭据，`--dry-run` 只预演。

### 3.6 示例

```bash
# 首次初始化（混合节点）
kubemono config validate -f cluster.yaml
kubemono bundle load /mnt/usb/kubemono/kubemono-v1.37.4-linux-amd64.tar.zst
kubemono init -f cluster.yaml

# 先预演：确认身份、角色、phase 顺序
kubemono init -f cluster.yaml --dry-run

# 初始化后签发 worker / master 凭证
kubemono token create
kubemono token create --role master

# 灾难恢复：确认要重建本机控制面
kubemono init -f cluster.yaml --force-init --allow-existing-cluster
```

### 3.7 常见错误

| 报错 | 原因与处理 |
|---|---|
| `cluster configuration ... not found: pass -f cluster.yaml` | 默认路径没有配置；用 `-f` 指定 |
| `init 只能在 master 上执行` | 本机角色不是 master；换节点或改 `roles` |
| `本机存在 etcd 数据（/var/lib/etcd）；确认要重建时使用 --force-init` | 先确认是否误跑；确要重建加 `--force-init`，或先 `reset` |
| `检测到已有 apiserver 可达（...）：本机可能已属于运行中的集群` | 正常情况应改用 `join`；确要重建加 `--allow-existing-cluster` |
| `集群尚未初始化：缺少 kube-system/kubemono-cluster` | `token create` 在尚未完成 `init` 的集群上执行；先完成初始化 |

---

## 4. `kubemono join`

### 4.1 使用场景

- 新增 worker 或 master；
- 节点重装后重新加入；
- 替换故障节点（先让旧节点 `reset --leave`，新节点用新的 join 命令）。

### 4.2 语法与前置条件

```bash
# worker
kubemono join [api-server-endpoint] --token <id.secret> \
  --discovery-token-ca-cert-hash sha256:<hex> [-f cluster.yaml] [flags]

# master / etcd：追加
kubemono join ... --control-plane --certificate-key <64 位 hex>
```

| 前置条件 | 说明 |
|---|---|
| 角色匹配 | 默认 worker；master/etcd 加 `--control-plane`，且必须带 `--certificate-key`。纯 worker 传 `--control-plane`/`--certificate-key` 会被拒绝 |
| 配置 | 本机必须在 `cluster.hosts[]` 中；否则用 `--node` 指定 |
| CA pin | 默认必须提供 `--discovery-token-ca-cert-hash`；无法提前获取时显式加 `--discovery-token-unsafe-skip-ca-verification` |
| 有效期 | bootstrap token 2h 内有效（硬上限 2h）；过期用 `kubemono token create`（worker）或 `kubemono token create --role master` 重新签发 |
| 集群可达 | join 需要访问 master 的 6443（或 `externalEndpoints`）；不可达时按 `--wait` 重试 |
| 权限 | root |
| 物料 | 本机需有对应 release 的物料（`bundle load`），否则 runtime phase 无法导入镜像 |

### 4.3 参数

| flag | 必填 | 默认值 | 作用 | 说明 |
|---|---|---|---|---|
| `--token` | **是**（或 `--token-file`） | 无 | bootstrap token | `<6 位 id>.<16 位 secret>` |
| `--token-file` | 否 | 无 | 从文件读取 bootstrap token | 避免 token 出现在 argv/`/proc/<pid>/cmdline`；与 `--token` 互斥 |
| `--control-plane` | 否 | false | 以 master/control-plane 身份加入 | 必须同时给 `--certificate-key` |
| `--certificate-key` | master 必填（或 `--certificate-key-file`） | 无 | 解密 `kubemono-certs` 证书包 | 64 位 hex；worker 不能带 |
| `--certificate-key-file` | 否 | 无 | 从文件读取 certificate-key | 与 `--certificate-key` 互斥 |
| `--discovery-token-ca-cert-hash` | 是（除非 skip） | 无 | CA 公钥 pin | 格式 `sha256:<hex>`，可重复 |
| `--discovery-token-unsafe-skip-ca-verification` | 否 | false | 跳过 CA pin 校验 | 不推荐；仅在无法提前获取 hash 时用 |
| `-f, --config` | 否 | `/usr/local/kubemono/cluster.yaml` | 集群配置 | 读不到直接失败 |
| `--node` | 否 | 空 | 指定本机配置身份 | 优先级最高 |
| `--allow-address-mismatch` | 否 | false | 允许 `--node` 地址不在本机网卡 | NAT |
| `--replace-pki` | 否 | false | 允许用集群证书包覆盖本机已有且不同的 CA 私钥 | 会让本机已签发证书失效 |
| `--wait` | 否 | `30m` | 等待集群可达的最长时间 | 只重试传输层错误，身份/JWS 错误立即失败 |
| `--dry-run` | 否 | false | 只打印计划 | 不取证书、不写文件 |
| `--data-dir` | 否 | `/usr/local/kubemono` | 数据目录 | |

`[api-server-endpoint]` 是可选位置参数（如 `10.0.0.1` 或 `10.0.0.1:6443`）；
不传时从 `cluster.yaml` 的 master / `externalEndpoints` 推导。

`join` **没有** `--kubeconfig`：加入过程中会先拿到 CA（worker）或证书包（master），
再用集群内身份完成后续 phase。

### 4.4 它做了什么

1. 解析身份、校验 join 凭证角色（`--control-plane`）；
2. `prepareJoin`：worker 从 `kube-public/cluster-info` 校验 CA 并取回集群 CA；
   master 用 `--certificate-key` 解密 `kube-system/kubemono-certs` 证书包；
3. 按角色执行 phase：以 §5.4 的 repair 表为基线，最前面加 `preflight`。
   **join 不执行 `cluster-init`**（加入者不重放集群初始化），其余与 repair 相同；
   worker 的 `local-proxy` 必须先于 `kubelet`：TLS Bootstrap 走 `127.0.0.1:6443`；
4. 成功后同样写 `node.yaml`、配置缓存与 `desired/` 快照，并执行 `postInstall` 钩子。

### 4.5 示例

```bash
# worker 加入（直接复制 kubemono token create 输出的命令）
kubemono join 10.0.0.1:6443 --token abcdef.0123456789abcdef \
  --discovery-token-ca-cert-hash sha256:... -f /usr/local/kubemono/cluster.yaml

# 第二台 master 加入（混合节点）
kubemono join 10.0.0.1:6443 --token abcdef.0123456789abcdef \
  --discovery-token-ca-cert-hash sha256:... \
  --control-plane --certificate-key <64 位 hex> -f /usr/local/kubemono/cluster.yaml

# 预演：只确认身份与 phase 顺序
kubemono join 10.0.0.1:6443 --token abcdef.0123456789abcdef \
  --discovery-token-ca-cert-hash sha256:... -f cluster.yaml --dry-run

# master 还没起来：多等 10 分钟
kubemono join 10.0.0.1:6443 --token abcdef.0123456789abcdef \
  --discovery-token-ca-cert-hash sha256:... -f cluster.yaml --wait 10m
```

### 4.6 常见错误

| 报错 | 原因与处理 |
|---|---|
| `加入集群需要 --token` | 没给 join 凭证；从 `kubemono token create` 输出复制命令 |
| `纯 worker 不能以 master 身份加入` | 在 worker 上用了 `--control-plane`；去掉它 |
| `纯 worker 不能携带 certificateKey` | 在 worker 上用了 `--certificate-key`；去掉它 |
| `master/etcd 加入需要 --control-plane 与 --certificate-key` | master 漏了 `--control-plane` 或 `--certificate-key` |
| `必须提供 --discovery-token-ca-cert-hash` | 没给 CA pin；从 `kubemono token create` 输出复制，或显式 `--discovery-token-unsafe-skip-ca-verification` |
| `等待集群加入应答超时` | master/apiserver 不可达或未就绪；检查 `--wait`、端口、`externalEndpoints` |
| `引导令牌无效` | token 抄错/损坏；重新签发 |
| `CA 私钥冲突: ... 已存在且与集群 CA 不同` | 本机残留旧 PKI；确认要重建本机 PKI 时加 `--replace-pki`，否则先 `reset` |

---

## 5. `kubemono node repair`

### 5.1 使用场景

- **应用 `cluster.yaml` 变更**：改节点标签/污点、kubelet 参数、控制面参数、网络参数、
  镜像仓库、CoreDNS 等，都用它幂等重跑；
- **安装中途失败后恢复**：同一个命令从断点继续，已完成的 phase 会被 `Check` 跳过；
- **只推某一个 phase**：改动面很小时用 `--only`，避免整表重跑；
- **只看差异不动手**：用 `--diff` 先确认会改哪些文件。

它不是 `init`/`join` 的替代：未安装节点（没有 `node.yaml`）会被拒绝。

### 5.2 语法与前置条件

```bash
kubemono node repair -f cluster.yaml [flags]
```

| 前置条件 | 说明 |
|---|---|
| 已安装 | 必须有 `/usr/local/kubemono/node.yaml`（`init`/`join` 写入） |
| 配置 | `cluster.yaml` 必须存在且可加载 |
| 权限 | root；master 上的集群级 phase 需要 admin 凭据 |
| 角色 | 按本机角色执行对应 phase 表；worker 上没有 `etcd`，master 上没有 `local-proxy` |

### 5.3 参数

| flag | 必填 | 默认值 | 作用 | 说明 |
|---|---|---|---|---|
| `-f, --config` | 否 | `/usr/local/kubemono/cluster.yaml` | 集群配置 | 日常修复通常显式给 `-f` |
| `--node` | 否 | 空 | 指定本机配置身份 | 与已安装身份不一致会拒绝，除非 `--rebind-identity` |
| `--only` | 否 | 空 | 只执行指定 phase | 逗号分隔/可重复；只能选**本机计划内且 `Applies` 为真**的 phase；顺序按计划而不是命令行；至少一个；不执行 `postInstall` 钩子，也不更新 desired 快照/cluster.yaml 缓存 |
| `--diff` | 否 | false | 只读比较期望状态与磁盘 | 打印缺失/内容/权限差异，Secret 只报状态不打印内容；不做任何写入；**不能与 `--only` 同用**；不要与 `--dry-run` 混用（`--dry-run` 会先短路，只剩计划输出） |
| `--rebind-identity` | 否 | false | 允许 `--node`/NodeName 改写本机已有身份 | 仅用于节点改名迁移 |
| `--allow-release-mismatch` | 否 | false | 允许用不同 release 的 kubemono 修复已安装节点 | 默认拒绝，避免半改节点 |
| `--exit-code` | 否 | false | `--diff` 发现差异时返回非 0 | 只能与 `--diff` 同用 |
| `--timeout` | 否 | `0`（不限制） | 整轮修复的最长时长 | 超时后当前 phase 中断并返回错误；phase 内部超时仍生效 |
| `--kubeconfig` | 否 | `/etc/kubernetes/admin.conf` | 集群级 phase 的凭据 | master 上默认即可；worker 上集群级对象由 master 维护 |
| `--dry-run` | 否 | false | 只打印计划 | 不写文件、不重启 |
| `--data-dir` | 否 | `/usr/local/kubemono` | 数据目录 | |

### 5.4 执行哪些 phase

`node repair` 的 phase 表按角色固定（顺序即依赖）：

| 角色 | phase 顺序 |
|---|---|
| `[master]`（纯控制面） | `host-init` → `certs` → `kubeconfig` → `etcd` → `etcd-health` → `control-plane` → `cluster-init` → `network` → `addon` |
| `[master, worker]` | `host-init` → `runtime` → `certs` → `kubeconfig` → `kubelet` → `etcd` → `etcd-health` → `control-plane` → `cluster-init` → `mark-control-plane` → `network` → `addon` |
| `[worker]` | `host-init` → `runtime` → `kubeconfig` → `local-proxy` → `kubelet` → `mark-control-plane` → `network` |

与 `init`/`join` 的关系：init 与 join 都在本表前加 `preflight`；join 不执行
`cluster-init`，其余相同。三者都不生成 join 材料（由 `kubemono token create` 负责），
已完成的 phase 由 `Check` 跳过。

phase 含义（也用于 `--only` 取值）：

| phase | 作用 |
|---|---|
| `host-init` | 内核模块、sysctl 等主机基线 |
| `runtime` | 容器运行时（containerd 或 custom CRI）与 CNI 插件二进制 |
| `certs` | PKI 生成/按配置重签、证书轮转 |
| `kubeconfig` | `/etc/kubernetes/*.conf` |
| `kubelet` | kubelet 配置与 unit |
| `etcd` / `etcd-health` | 本地 etcd 配置、成员与健康检查（仅本地 etcd） |
| `control-plane` | apiserver/kcm/scheduler 静态 Pod 清单与重启 |
| `cluster-init` | 引导 RBAC、`cluster-info`、集群状态 ConfigMap（repair 不碰令牌与证书包） |
| `mark-control-plane` | 把 `hosts[].labels` / `taints` 写到 Node（只增改，不删除配置里已移除的项） |
| `local-proxy` | worker 本地 haproxy（转发 `127.0.0.1:6443`） |
| `network` | CNI 配置文件、kube-router / kube-proxy 对象 |
| `addon` | CoreDNS |

### 5.5 `--only` 的规则

```bash
# 只推 worker 的本地代理（改完 master 列表后的典型用法）
kubemono node repair -f cluster.yaml --only local-proxy

# 多选：逗号分隔或重复给出，实际顺序仍按计划
kubemono node repair -f cluster.yaml --only runtime --only kubelet
```

- 只接受**本机计划内**的 phase；非法名称会报错并列出本机可用的 phase；
- 顺序由计划决定，命令行顺序不生效（存在依赖，例如 `local-proxy` 必须先于 `kubelet`）；
- `init`/`join` 不支持 `--only`；
- 带 `--only` 时不会执行 `postInstall` 钩子（局部修复不应触发业务动作）。

### 5.6 `--diff` 的语义

```bash
kubemono node repair -f cluster.yaml --diff
```

- 只渲染本机期望状态并与磁盘比较：缺失、内容不同、权限不同；
- Secret 类内容只报告状态，不打印明文；
- 不做任何写入，不重启组件；
- 需要已安装节点（同样是 `node repair` 的前置条件）；
- 与 `--only` 互斥：`--diff` 永远比较整机受管文件。

### 5.7 节点改名（`--rebind-identity`）

```bash
# cluster.yaml 中把 hosts[].name 从 w1 改成 worker-a 后
kubemono node repair -f cluster.yaml --node worker-a --rebind-identity
```

不加 `--rebind-identity` 而 `--node` 与 `node.yaml` 记录不一致时会被拒绝，避免误改身份。

### 5.8 示例

```bash
# 1) 先看会改什么
kubemono node repair -f cluster.yaml --diff

# 2) 再应用（幂等，可重复执行）
kubemono node repair -f cluster.yaml

# 3) 只修运行时（例如换了 containerd 镜像仓库配置）
kubemono node repair -f cluster.yaml --only runtime

# 4) 限制整轮时长，避免卡在等待循环
kubemono node repair -f cluster.yaml --timeout 30m
```

### 5.9 注意

- 集群级对象（kube-router、kube-proxy、CoreDNS）由 **master** 上的 `repair` 收敛；
  worker 上这些 phase 会跳过并说明原因；
- master 不可达时，本机 phase 会先执行完，随后在集群级 phase 失败并给出客户端错误
  （不会假装成功）；
- 修复会先比较内容，只有真的不同才写文件/重启；清理生成文件里的注释这类一次性变更，
  只会在第一次修复时触发重启；
- `runtime.custom` 与 `containerd` 互切属于不可原地变更，见 §14。

---

## 6. `kubemono cert`

### 6.1 三个子命令

| 子命令 | 用途 | 改文件 | 需要 root | 需要集群 |
|---|---|---|---|---|
| `check-expiration` | 扫描本机证书，报告剩余有效期 | 否 | 通常需要（读 PKI） | 否 |
| `renew` | 重签本机叶子证书并重启组件 | 是 | 是 | 可选（缺配置走应急路径） |
| `show` | 只读解析 PEM 证书（含链） | 否 | 否 | 否 |

### 6.2 `cert check-expiration`

```bash
kubemono cert check-expiration [flags]
```

| flag | 必填 | 默认值 | 作用 | 说明 |
|---|---|---|---|---|
| `--warn-before` | 否 | 30 天 | 告警窗口 | 接受 Go duration（`720h`）或天数（`30d`）；非法/非正数报错 |
| `--fail-on-warn` | 否 | false | 进入告警窗口也返回非零 | 用于 cron/监控；已过期/无法解析/不一致会非零 |
| `--output` | 否 | `table` | `table\|json\|yaml` | JSON 是证书状态数组 |
| `--data-dir` | 否 | `/usr/local/kubemono` | 数据目录 | |

扫描范围与判定：

- `/etc/kubernetes/pki/*.crt`、`/etc/kubernetes/pki/etcd/*.crt`、`/etc/kubernetes/*.conf` 引用的证书；
- 结果按「证书身份（路径 + 指纹）」聚合，同一张 CA 被多个 kubeconfig 引用只出一行，
  `USED BY` 列给出引用数量；
- kubeconfig 引用与 kubemono 期望不一致（路径不对、内嵌证书不同、集群 CA 不同）会单独成行并标
  `mismatch`，表格下方给出原因；
- 表格列：`SOURCE`、`PATH`、`CN`、`NOT AFTER`、`REMAINING`、`STATUS`、`USED BY`。

```bash
# 人工巡检
kubemono cert check-expiration

# 监控：进入 30 天窗口就告警；放宽到 45 天
kubemono cert check-expiration --fail-on-warn --warn-before 45d

# 机器可读
kubemono cert check-expiration --output json | jq '.[] | select(.expired)'
```

### 6.3 `cert renew`

```bash
kubemono cert renew -f cluster.yaml [flags]
```

| flag | 必填 | 默认值 | 作用 | 说明 |
|---|---|---|---|---|
| `--cert` | 否 | 空（全部） | 只续期指定证书 | 可重复/逗号分隔；取值是 catalogue ID、证书文件名或去掉 `.crt` 的名字；**只缩小范围，不改变有效期判定**（窗口外仍会跳过，需要强制时加 `--all`）；点名但本机不存在会失败 |
| `--all` | 否 | false | 重签全部证书 | 默认只重签 30 天内到期或已过期的；与 `--cert` 同用表示「强制重签点名的证书」 |
| `-y, --assume-yes` | 否 | false | 跳过确认 | 只影响确认，不改变续期范围 |
| `--dry-run` | 否 | false | 只报告将续期的证书 | 不写文件、不重启；仍会校验 CA 指纹 |
| `-f, --config` | 否 | `/usr/local/kubemono/cluster.yaml` | 集群配置 | 提供 `controlPlane.pki.certValidity` 与角色判断；读不到时**警告后按默认值继续**（应急路径） |
| `--output` | 否 | `table` | `table\|json\|yaml` | JSON 报告含 `results`/`renewed`/`skipped`/`failed`/`restarted` |
| `--data-dir` | 否 | `/usr/local/kubemono` | 数据目录 | |

可用 `--cert` 取值：

```text
apiserver  apiserver-kubelet-client  apiserver-etcd-client  front-proxy-client
controller-manager  scheduler  admin  super-admin  kubelet-client
etcd-server  etcd-peer  etcd-kubemono-client
```

行为要点：

- **只能在持有 CA 私钥的 master 上执行**（有 `cluster.yaml` 时会校验角色）；worker 的客户端证书
  由 TLS Bootstrap 自动轮换，不在此处理；
- 默认只处理进入续期窗口（30 天）或已过期的证书；`--all` 强制全部重签，
  `--cert` 只缩小范围（`--cert` 与 `--all` 同用才强制重签点名的那几张）；
- 重签保留原证书的 Subject、SAN 与 EKU；CA 与 ServiceAccount 密钥**不变**
  （续期前后会校验 CA 指纹，变化即报错）；
- 有效期取 `controlPlane.pki.certValidity`（缺配置时 365 天）；
- 续期后按证书影响滚动重启组件：systemd 组件用 `systemctl restart` 并等待健康
  （最长 90s）；`local-proxy` 通过改静态 Pod 注解让 kubelet 重建；
- kubeconfig 引用证书文件而不是内嵌，因此续期不需要重写 kubeconfig；
- `--dry-run` 可以放进 cron 做预演；非交互环境执行真实续期必须显式加 `-y`。

```bash
# 1) 预演
kubemono cert renew -f cluster.yaml --dry-run

# 2) 只续期 apiserver 与 etcd-server
kubemono cert renew -f cluster.yaml --cert apiserver,etcd-server -y

# 3) 全部重签（例如有效期策略变化后）
kubemono cert renew -f cluster.yaml --all
```

常见错误：

| 报错 | 原因与处理 |
|---|---|
| `本机没有 CA 私钥（/etc/kubernetes/pki），无法续期` | 在 worker 上执行了；到 master 上执行 |
| `cert renew 只能在 master 上执行（需要 CA 私钥）` | 同上；或本机在配置里不是 master |
| `未知证书 "..."；可用 --cert 指定 catalogue ID 或证书文件名` | 拼写错误；用上文列表中的 ID |
| 组件重启后 90s 未就绪 | 看该组件日志；证书已写入，可手工重启后继续 |

修改 `controlPlane.certSANs` / `externalEndpoints` 后**不要**用 `cert renew`（它保留原 SAN），
应执行 `node repair`，由 master 的 `certs` phase 按新配置重签。

### 6.4 `cert show`

```bash
kubemono cert show <cert.pem> [cert.pem ...] [--output table|json|yaml]
```

- 至少一个参数；同一文件里的证书链会全部展开；
- 只读，不读私钥、不改文件；
- 输出 Subject、Issuer、自签标记、序列号、有效期、CA/路径长度、KeyUsage/EKU、
  DNS/IP/URI/Email SAN、签名算法、公钥算法、SHA256 指纹。

```bash
kubemono cert show /etc/kubernetes/pki/ca.crt
kubemono cert show /etc/kubernetes/pki/apiserver.crt --output json | jq '.[0].dnsNames'
```

---

## 6.5 `kubemono cert timer`

可选地安装 systemd timer，每天执行一次 `kubemono cert renew`；它不是常驻守护进程，
续期逻辑仍是同一条命令。

```bash
kubemono cert timer install    # 仅 master；写入 service/timer 并 enable --now
kubemono cert timer install --dry-run
kubemono cert timer remove     # 停止/禁用并删除 unit
```

- 只允许已安装的 master（worker 没有 CA 私钥）；
- service 使用当前 kubemono 可执行文件与 `-f` 指定的 cluster.yaml；
- timer 时间 `03:17`，随机延迟最多 30 分钟，`Persistent=true`；
- 续期失败会保留待重启标记，下次 timer 或人工 `cert renew` 重试；
- 安装/移除需要 root 与 systemd；`kubemono status` 会显示该 timer 的 active/enabled/result/nextRun，`status --check` 在 `kubemono-cert-renew.service` 上次 `Result=failed` 时返回非 0。
- 升级 kubemono 后请重跑一次 `cert timer install`：它会用当前二进制/配置路径重写 unit，并给 service 加上非交互所需的 `-y`（旧 unit 没有会一直按“取消”处理）。

## 7. `kubemono pki issue`

### 7.1 使用场景

- 给外部 etcd、监控、测试环境签发客户端/服务端证书；
- 用企业 CA 或集群 CA 手工补一张证书；
- 需要可复现的离线签发，不经过 apiserver。

它**不**创建 CA、**不**写 `/etc/kubernetes`、**不**管理集群 PKI；集群证书仍由
`certs` phase 与 `cert renew` 管理。

### 7.2 参数

```bash
kubemono pki issue --ca <ca.crt> --ca-key <ca.key> --cn <name> [flags]
```

| flag | 必填 | 默认值 | 作用 | 说明 |
|---|---|---|---|---|
| `--ca` | **是** | 无 | CA 证书路径 | 与 `--ca-key` 必须成对 |
| `--ca-key` | **是** | 无 | CA 私钥路径 | |
| `--cn` | **是** | 无 | 证书 CN | 同时作为输出文件基名 `<cn>.crt` / `<cn>.key`；不能含 `/`、`\` |
| `--profile` | 否 | `server` | 用途 `server\|client\|peer` | `server`=ServerAuth，`client`=ClientAuth，`peer`=两者兼有 |
| `--san` | 条件 | 空 | SAN，DNS 或 IP | `server`/`peer` 至少一个；`client` 可省略；不支持 URI/Email（`uri:`/`email:` 会报错） |
| `--organization` | 否 | 空 | 证书 O | 可重复 |
| `--days` | 否 | `365` | 有效期天数 | 自动钳制到 CA 到期时间；CA 已过期直接报错 |
| `--out-dir` | 否 | `.` | 输出目录 | 以 0700 创建 |
| `--output` | 否 | `table` | `table\|json\|yaml` | JSON 含 `certPath`/`keyPath`/`notAfter`/`fingerprint` |

约束：

- **拒绝覆盖**已有证书/私钥（存在即报错，需先移走或改 `--cn`/`--out-dir`）；
- 私钥文件权限 0600，证书 0644。

```bash
# 外部 etcd 客户端证书
kubemono pki issue \
  --ca /etc/kubernetes/pki/etcd/ca.crt \
  --ca-key /etc/kubernetes/pki/etcd/ca.key \
  --cn kubemono-etcd-client --profile client --days 365

# Ingress/API 服务端证书
kubemono pki issue \
  --ca ca.crt --ca-key ca.key \
  --cn api.example.com --profile server \
  --san api.example.com --san 10.0.0.10 --days 825 \
  --out-dir ./certs
```


---

## 8. `kubemono bundle`

物料生命周期：

```text
bundle pack（构建机，造包 + .sha256）
   → 拷贝到目标机
   → bundle verify <archive>（可选但推荐）
   → bundle load <archive>（导入 /usr/local/kubemono/artifacts/<release>）
   → init / join / node repair 使用物料
```

`make dist` 只是在构建机上跑同一条 `bundle pack`。

物料里的组件版本、下载地址与镜像引用由 BOM 决定；如何在版本/URL 上做定制见 [`bom.md`](bom.md)。

### 8.1 `bundle pack`

按 BOM 获取组件物料并写出可直接 `load` 的归档（唯一造包入口）。

| flag | 必填 | 默认值 | 作用 | 说明 |
|---|---|---|---|---|
| `--arch` | 否 | `linux/amd64` | 目标架构 | 一次一个；接受 `amd64` 或 `linux/amd64`；必须在 BOM 的 arch 列表内 |
| `--images` | 否 | false | 生成含 OCI 镜像的完整包 | 默认是轻量包（只有组件二进制） |
| `--fetch-images` | 否 | true | 缺镜像时联网拉取 | 只能配合 `--images`；`--fetch-images=false` 只用本地镜像缓存 |
| `--offline` | 否 | false | 只用本地缓存，缺失即报错 | 与 `--refresh` 冲突；离线模式下不会拉取镜像 |
| `--refresh` | 否 | false | 忽略已有组件缓存重新下载 | 镜像仍按 OCI layout 判断 |
| `--created` | 否 | 空 | 覆盖归档创建时间（RFC3339） | 用于可复现构建；未设置时读 `SOURCE_DATE_EPOCH`（Unix 秒），否则用当前时间 |
| `--cache-dir` | 否 | 用户缓存目录下的 `kubemono/pack` | 下载缓存根目录 | 实际按 `<cache-dir>/<release>/` 隔离 |
| `--out-dir` | 否 | `dist` | 归档输出目录 | 只放最终归档与 `.sha256`；不能与缓存目录互相包含 |
| `--bom` | 否 | 内置 BOM | 替换组件列表与获取 URL | 自定义发行版/测试 |
| `-f, --config` | 否 | 默认路径 | 版本/镜像覆盖 | 没有配置时警告并按内嵌 BOM 打包；显式给 `-f` 读不到会失败 |
| `--dry-run` | 否 | false | 打印将使用的组件/镜像与输出路径 | 不联网、不写盘（本命令的 `--dry-run` 是本地 flag，语义相同） |
| `--output` | 否 | `table` | `table\|json\|yaml` | JSON 含 `archive`/`sha256`/`size` |
| `--data-dir` | 否 | `/usr/local/kubemono` | 数据目录 | |

产物命名：

```text
kubemono-<release>-linux-<arch>.tar.zst          # 完整包（含镜像）
kubemono-<release>-linux-<arch>-lite.tar.zst     # 轻量包（--images 未开）
```

```bash
kubemono bundle pack                                   # 轻量包
kubemono bundle pack --images                          # 完整包
kubemono bundle pack --images --fetch-images=false     # 只用本地镜像缓存
kubemono bundle pack --arch linux/arm64
kubemono bundle pack -f cluster.yaml --images --out-dir /mnt/usb
kubemono bundle pack -f cluster.yaml --bom ./bom.yaml --offline
```

### 8.2 `bundle list`

```bash
kubemono bundle list [<archive>] [flags]
```

- **无参数（计划模式）**：按 BOM/`cluster.yaml` 列出组件与版本，并标记本地物料库是否已具备；
- **带归档参数（归档模式）**：只读归档的 `meta.json`，不解包、不写文件；此模式**不接受**
  `-f/--config`、`--arch`、`--bom`。

| flag | 必填 | 默认值 | 作用 | 说明 |
|---|---|---|---|---|
| `--category` | 否 | 空 | 只列某个类别 | 有效值：`k8s`、`etcd`、`runtime`、`cni`、`network`、`addon`、`lb`、`image` |
| `--images` | 否 | false | 列镜像而不是组件 | 归档模式下不能与 `--category` 同用 |
| `--arch` | 否 | `linux/amd64` | 计划模式的目标架构 | 一次一个；归档模式禁用 |
| `--bom` | 否 | 内置 | 覆盖 BOM | 归档模式禁用 |
| `-f, --config` | 否 | 默认路径 | 版本覆盖 | 归档模式禁用 |
| `--output` | 否 | `table` | 机器可读输出 | |
| `--data-dir` | 否 | `/usr/local/kubemono` | 数据目录 | 决定物料库位置 |

状态列：`✓`（已具备）、`缺失`、`不在 meta`（文件存在但未登记）、`-`（无二进制）。

```bash
kubemono bundle list
kubemono bundle list --category runtime
kubemono bundle list --images
kubemono bundle list dist/kubemono-v1.37.4-linux-amd64.tar.zst --images
```


### 8.3 `bundle load`

```bash
kubemono bundle load <archive> [flags]
```

| flag | 必填 | 默认值 | 作用 | 说明 |
|---|---|---|---|---|
| `<archive>` | **是** | 无 | `.tar.zst` 归档 | 必须包含 `meta.json` |
| `--sha256` | 否 | 读同目录 `.sha256` | 期望摘要 | 两者都没有时**警告并跳过**摘要校验 |
| `--no-arch` | 否 | false | 跳过目标架构检查 | 跨架构导入/排障用 |
| `--overwrite` | 否 | false | 强制重新解包 | 默认是幂等的：同摘要已安装会跳过；不同摘要会替换目标目录（旧目录备份为 `<release>.old` 后在成功时删除） |
| `--dry-run` | 否 | false | 只报告写入位置 | 仍会校验摘要 |
| `--output` | 否 | `table` | 机器可读输出 | |
| `--data-dir` | 否 | `/usr/local/kubemono` | 数据目录 | 导入到 `<data-dir>/artifacts/<release>` |

- 只做导入：release/arch/images 以归档 `meta.json` 为准，不联网、不构建、不选镜像；
- 本机存在 `cluster.yaml` 时只读对比 release 并提示差异，**不会**拒绝导入；
- 多个 release 可以在同一物料库并存。

```bash
kubemono bundle load dist/kubemono-v1.37.4-linux-amd64.tar.zst
kubemono bundle load /mnt/usb/pkg.tar.zst --sha256 <sha256>
kubemono bundle load pkg.tar.zst --no-arch --overwrite
```

### 8.4 `bundle verify`

```bash
kubemono bundle verify [<archive>] [flags]
```

| flag | 必填 | 默认值 | 作用 | 说明 |
|---|---|---|---|---|
| `[<archive>]` | 否 | 空 | 归档模式 | 只读归档、不解包；此模式禁用 `-f`/`--release` |
| `--release` | 否 | 自动解析 | 库模式要检查的 release | 解析顺序：`--release` → `cluster.yaml` 生效 release → 物料库中唯一的目录（多个则报错） |
| `--sha256` | 否 | 读 `.sha256` | 归档期望摘要 | 只对归档模式有效 |
| `--no-hash` | 否 | false | 跳过逐文件 sha256 | 只查存在、大小、权限、架构 |
| `--no-arch` | 否 | false | 跳过可执行文件架构检查 | |
| `-f, --config` | 否 | 默认路径 | 库模式定位 release | 归档模式禁用 |
| `--output` | 否 | `table` | 机器可读报告 | |
| `--data-dir` | 否 | `/usr/local/kubemono` | 数据目录 | |

校验内容：文件是否存在、大小、权限、逐文件 sha256（meta 里没有 sha256 的文件只校验大小与权限）、
可执行文件架构。任何一项不通过 → 退出码 1。

```bash
kubemono bundle verify dist/kubemono-v1.37.4-linux-amd64.tar.zst
kubemono bundle verify                      # 检查本机物料库（release 自动解析；多个时用 -f/--release）
kubemono bundle verify --release v1.37.4 --no-hash
```

---

## 9. `kubemono config`

### 9.1 `config default`

```bash
kubemono config default [--output yaml|json]
```

| flag | 必填 | 默认值 | 作用 | 说明 |
|---|---|---|---|---|
| `--output` | 否 | `yaml` | 配置文档格式 | 只接受 `yaml\|json`；这里的 `--output` **不是**表格格式，也不接受 `table` |

- 输出一份补齐全部非空默认值的完整配置 + 单 master 占位拓扑；
- `cluster.hosts` 是占位值，安装前必须替换；
- YAML 带中文分段注释；JSON 是纯文档内容。

```bash
kubemono config default > cluster.yaml
kubemono config default --output json > cluster.json
```

### 9.2 `config validate`

```bash
kubemono config validate -f cluster.yaml
```

| flag | 必填 | 默认值 | 作用 |
|---|---|---|---|
| `-f, --config` | 否 | `/usr/local/kubemono/cluster.yaml` | 要校验的文件 |
| `--output` | 否 | `table` | `table` 打印摘要；`json/yaml` 输出规范化后的完整配置 |
| `--data-dir` | 否 | `/usr/local/kubemono` | 数据目录 |

校验内容：

- 严格 YAML：**未知字段直接拒绝**；
- 字段默认值、取值范围、相互约束（CIDR、端口、路径、证书 SAN、运行时、网络、etcd 等）；
- 版本/镜像覆盖：未知组件名报错；k8s 版本任意但非默认只告警；
- 错误**一次给全**，不是遇到第一个就退出。

`table` 输出摘要字段：`masters`、`workers`、`etcd`、`network plugin`、`service proxy`、
`podCIDR`、`serviceCIDR`、`clusterDNS`、`runtime`，最后一行 `[ok] <path> 校验通过`。

```bash
kubemono config validate -f cluster.yaml
kubemono config validate -f cluster.yaml --output json | jq '.cluster.hosts'
```

---

## 10. `kubemono reset`

### 10.1 使用场景

- **节点退出集群**：`reset --leave`（cordon → 停 kubelet → 删 Node → 移除 etcd 成员 → 本机重置）；
- **重装节点前清理**：`reset`（保留物料/备份/状态）或 `reset --purge`（全部清掉）；
- **演练**：`reset --dry-run` 只打印将要删除/还原的内容。

### 10.2 参数

| flag | 必填 | 默认值 | 作用 | 说明 |
|---|---|---|---|---|
| `--leave` | 否 | false | 先退群再重置本机 | 需要本机 `node.yaml` 与 admin 凭据；worker 没有 admin.conf 时按报错里的手工步骤处理 |
| `--force-leave` | 否 | false | 跳过 cordon 失败与 etcd 法定人数校验 | **只在 `--leave` 时可用**；单成员 etcd 会跳过成员移除并继续清理 |
| `--kubeconfig` | 否 | `/etc/kubernetes/admin.conf` | 退群使用的凭据 | **只在 `--leave` 时可用**；非 `--leave` 使用会直接报错 |
| `--purge` | 否 | false | 彻底清理：运行二进制、CNI 插件、containerd 配置/数据、`/usr/local/kubemono`、运行时 unit | 与 `--keep-data`/`--keep-hostinit` 互斥 |
| `--keep-data` | 否 | false | 保留 etcd 与 kubelet 数据目录 | 与 `--purge` 互斥 |
| `--keep-hostinit` | 否 | false | 不还原主机初始化改动（内核模块/sysctl） | 与 `--purge` 互斥 |
| `-y, --assume-yes` | 否 | false | 跳过确认 | 非交互环境必须加 |
| `--dry-run` | 否 | false | 只打印清单 | 跳过确认，不备份、不删除 |
| `-f, --config` | 否 | 默认路径 | 集群配置 | 决定运行时类型、etcd 数据目录与退群角色；**显式给出但读不到时直接报错**；省略且默认文件不存在时才按默认值继续 |
| `--data-dir` | 否 | `/usr/local/kubemono` | 数据目录 | 备份与清理根目录 |

### 10.3 它做了什么

五个步骤（顺序固定）：

| 步骤 | 内容 |
|---|---|
| `control-plane` | 停止并禁用控制面 unit |
| `kubelet` | 停止 kubelet、用 ctr 停止容器、卸载 kubelet 数据目录下的挂载点 |
| `network` | 清理 kubemono 创建的 iptables/ipvs 规则、网络接口、CNI 配置 |
| `data` | 先把 PKI 与 `/etc/kubernetes/encryption`（若存在）备份到 `<data-dir>/backup/<timestamp>/`，再删除 `/etc/kubernetes` 与数据目录 |
| `runtime` | 还原主机初始化（`--purge` 时最后删除 `/usr/local/kubemono`） |

普通 `reset` 的删除/保留边界：

| 删除 | 保留 |
|---|---|
| `/etc/kubernetes` | `/usr/local/bin` 下的运行二进制 |
| `node.yaml`、`cluster.yaml`、`desired/` | `/usr/local/kubemono/artifacts`、`backup/`、`state/`、`hostinit.json` |
| `/var/lib/kubelet`、`/var/lib/etcd`（除非 `--keep-data`） | `/opt/cni/bin` 的 CNI 插件 |
| 运行时/控制面 unit 文件；`~/.kube/config` 中指向 admin.conf 的软链 | 用户自己的 kubeconfig |

`--purge` 额外删除：`/usr/local/bin` 中 BOM 声明的二进制、`/opt/cni/bin` 中 kubemono 安装的插件、
`/etc/cni/net.d`、`/var/lib/cni`、`/etc/containerd`、containerd 数据目录、`/var/log/kubernetes`
（审计日志）、整个 `/usr/local/kubemono`、运行时 unit。
备份先写到 `<data-dir>/backup/<timestamp>/`；**purge 成功后备份随根目录一起删除**，失败时保留用于恢复。

containerd 之外的运行时：

- `runtime.custom` 时，`--purge` 停止的 unit 是 `node.runtime.custom.serviceName`；
- 自定义运行时的数据目录未知，kubemono **不会**猜测删除（`ContainerdDataDirFor` 返回空）。

`--leave` 的顺序：cordon → 停 kubelet（避免重新注册）→ 删除 Node → 提示从 `cluster.yaml`
删除条目 → 移除 etcd 成员（先校验法定人数）→ 本机 reset。**驱逐**需要你在执行前自行
`kubectl drain`；`--leave` 只做 cordon。

### 10.4 示例

```bash
# 1) 先预览
kubemono reset --dry-run
kubemono reset --leave --dry-run

# 2) 节点退出集群（master）
kubemono reset --leave -y

# 3) 节点退出集群（worker，无 admin.conf）
#    先在 master 上：kubectl drain <node> --ignore-daemonsets --delete-emptydir-data
#    再在 master 上：kubectl delete node <node>（etcd 成员从 master 手动移除）
#    最后在 worker 上：
kubemono reset -y

# 4) 重装前彻底清理（--purge 与 keep 开关互斥）
kubemono reset --purge -y

#    只清配置、保留 etcd/kubelet 数据目录
kubemono reset --keep-data -y

# 5) 本机 apiserver 已坏，用远端 master 的凭据退群
kubemono reset --leave --kubeconfig /root/remote-admin.conf -y
```

### 10.5 常见错误

| 报错 | 原因与处理 |
|---|---|
| `--purge 会删除二进制、数据目录和主机改动，不能与 --keep-data/--keep-hostinit 同时使用` | 去掉 keep 开关或去掉 `--purge` |
| `--kubeconfig 只在 --leave 时使用` | 不退群的本机重置不需要凭据，去掉该 flag |
| `--force-leave 只在 --leave 时使用` | 同上 |
| `需要 master 上的 admin kubeconfig：...` | 在 master 上执行，或用 `--kubeconfig`；worker 按手工步骤退群 |
| `无法确定本机节点名：.../node.yaml 不存在或不含 nodeName` | `--leave` 需要已安装节点；先确认 `node.yaml` |
| `cordon 节点 ... 失败（--force-leave 可跳过）` | 节点已损坏时加 `--force-leave` |
| `已取消` | 非交互环境没加 `-y` |

---

## 10.6 `kubemono status`

只读汇总本机状态，不修改任何文件；`--output json|yaml` 可接监控/工单系统。

```bash
kubemono status
kubemono status -o json | jq '.problems, .pendingRestarts, .desiredDrift, .certs'
kubemono status --check          # 有问题返回非 0，可接监控/巡检
```

包含：身份（configName/nodeName/address/roles）、installed/release、clusterID、
desired 快照漂移、phase 最近结果（来自 `state/install.json`）、待重启标记、
证书到期汇总、systemd unit 的 active/enabled/required/result/nextRun，以及（有 kubeconfig 时）Node Ready。

`--check` 的阻塞项：未安装、desired 漂移、待重启、证书过期、Node 未 Ready、
本机角色要求但未运行/未启用（开机）的 unit、`kubemono-cert-renew.service` 上次 `Result=failed`。
`expiring` 只作为警告，不阻塞（timer/人工续期会处理）。

flag：`-f/--config`、`--node`、`--allow-address-mismatch`、`--output`、`--kubeconfig`、`--data-dir`、`--check`。
它不做 `node repair --diff` 的“当前配置 vs 磁盘”比较；两者互补。

## 11. `whoami` 与 `version`

### 11.1 `kubemono whoami`

```bash
kubemono whoami [-f cluster.yaml] [--node <name>] [--output table|json|yaml]
```

只读解析 `cluster.yaml` 与本机 `node.yaml`，打印本机被识别成哪个节点；不写文件、不访问 apiserver。

| 字段 | 含义 |
|---|---|
| `configName` | 匹配到的 `hosts[].name` |
| `nodeName` | 按 `nodeName` 策略 + 当前 hostname 计算出的实际 NodeName |
| `已安装名` / `installedNodeName` | `node.yaml` 里记录的安装时 NodeName |
| `address` | 匹配到的节点地址 |
| `roles` | `master` / `worker` |
| `installed` | 是否有 `node.yaml` |
| `clusterID` / `release` | `node.yaml` 中记录的集群身份与版本 |

排查「`--node` 覆盖、NAT 地址不匹配、`node.yaml` 与配置漂移」时先看这里：

```bash
kubemono whoami -f cluster.yaml
kubemono whoami -f cluster.yaml --node w1 --allow-address-mismatch
```

### 11.2 `kubemono version`

```bash
kubemono version [--short] [--output table|json|yaml]
```

| flag | 必填 | 默认值 | 作用 |
|---|---|---|---|
| `--short` | 否 | false | 只输出 release 号（适合脚本）；与显式 `--output` 互斥 |
| `--output` | 否 | `table` | `table\|json\|yaml` |

输出：kubemono release、Kubernetes 版本、Go 版本、Commit、构建时间、OS/Arch，以及
「Pinned component versions」清单。

注意：`version` 读取的是**二进制内嵌 BOM**，不读取 `cluster.yaml` 的
`kubernetesVersion`/`images.*` 覆盖。集群配置里声明的版本看
`kubemono config validate -f cluster.yaml --output json` 的 `kubernetesVersion`；
节点上的实际版本看组件本身（例如 `kubectl version`、`kubelet --version`）。

---

## 12. 命令 × 共享 flag 矩阵

`✓` 表示该命令注册了此 flag；`—` 表示不支持（传入会报 `unknown flag`）。

| 命令 | `-f` | `--data-dir` | `--node` | `--allow-address-mismatch` | `-y` | `--dry-run` | `--wait` | `--kubeconfig` | `--output` |
|---|---|---|---|---|---|---|---|---|---|
| `init` | ✓ | ✓ | ✓ | ✓ | — | ✓ | — | — | — |
| `join` | ✓ | ✓ | ✓ | ✓ | — | ✓ | ✓ | — | — |
| `node repair` | ✓ | ✓ | ✓ | — | — | ✓ | — | ✓ | — |
| `status` | ✓ | ✓ | ✓ | ✓ | — | — | — | ✓ | ✓ |
| `config default` | — | — | — | — | — | — | — | — | ✓（yaml\|json） |
| `config validate` | ✓ | ✓ | — | — | — | — | — | — | ✓ |
| `cert check-expiration` | — | ✓ | — | — | — | — | — | — | ✓ |
| `cert renew` | ✓ | ✓ | — | — | ✓ | ✓ | — | — | ✓ |
| `cert show` | — | — | — | — | — | — | — | — | ✓ |
| `pki issue` | — | — | — | — | — | — | — | — | ✓ |
| `bundle pack` | ✓ | ✓ | — | — | — | ✓ | — | — | ✓ |
| `bundle list` | ✓ | ✓ | — | — | — | — | — | — | ✓ |
| `bundle load` | — | ✓ | — | — | — | ✓ | — | — | ✓ |
| `bundle verify` | ✓ | ✓ | — | — | — | — | — | — | ✓ |

命令特有 flag：`status --check`、`cert timer install/remove`、`node repair --diff/--exit-code` 等见对应章节。
| `reset` | ✓ | ✓ | — | — | ✓ | ✓ | — | ✓（仅 `--leave`） | — |
| `whoami` | ✓ | ✓ | ✓ | ✓ | — | — | — | — | ✓ |
| `version` | — | — | — | — | — | — | — | — | ✓ |

补充：

- `-v/--verbose` 是全局 flag，上表不重复；
- `bundle pack` 的 `--dry-run` 是命令本地 flag，但与共享 `--dry-run` 语义一致；
- `bundle list` 在**归档模式**下额外禁用 `-f`、`--arch`、`--bom`；`bundle verify` 归档模式禁用
  `-f`、`--release`。

---

## 13. 端到端流程

### 13.1 全新集群

完整步骤（造包 → 每台节点 load → 首台 init → token create → 其余 join）见
[README §3](../README.md) 与 [README §8](../README.md)（离线）；这里不重复抄命令。

### 13.2 日常变更（改 `cluster.yaml`）

```bash
# 1) 校验配置
kubemono config validate -f cluster.yaml

# 2) 拷到目标节点，先看差异
kubemono node repair -f cluster.yaml --diff

# 3) 应用
kubemono node repair -f cluster.yaml

# 4) 只改了一个面时，收敛到单个 phase
kubemono node repair -f cluster.yaml --only runtime      # 运行时/镜像仓库
kubemono node repair -f cluster.yaml --only kubelet      # kubelet 参数
kubemono node repair -f cluster.yaml --only network      # CNI/kube-proxy
```

顺序建议：先 master 后 worker；集群级对象（network/addon）由 master 收敛。

### 13.3 证书运维

```bash
kubemono cert check-expiration                    # 巡检
kubemono cert renew -f cluster.yaml --dry-run     # 预演
kubemono cert renew -f cluster.yaml               # 续期 + 滚动重启
kubemono cert renew -f cluster.yaml --cert apiserver,etcd-server -y   # 指定证书
```

改 `certSANs`/`externalEndpoints` 不走 `cert renew`，走：

```bash
kubemono node repair -f cluster.yaml --only certs   # master
```

### 13.4 扩容 / 替换节点

```bash
# 扩容：改 cluster.yaml 加 host 条目 → validate → 新节点 join
kubemono config validate -f cluster.yaml
kubemono join <endpoint> --token <token> --discovery-token-ca-cert-hash <hash> -f cluster.yaml

# 替换：旧节点先 drain 再退群
kubectl drain <old-node> --ignore-daemonsets --delete-emptydir-data
kubemono reset --leave -y            # 在旧节点上（worker 无凭据则按 §10.4 手工步骤）
# 从 cluster.yaml 删除旧条目 → validate → 新节点用新 join 命令加入
```

### 13.5 离线环境

`bundle pack --images`、`bundle verify/load` 的完整流程见 [README §8](../README.md)
与 [`bom.md`](bom.md)；命令 flag 见本文件 §8。

### 13.6 故障恢复速查

| 症状 | 命令 |
|---|---|
| join 命令过期或丢失 | `kubemono token create`（worker）或 `kubemono token create --role master`（master） |
| 节点装到一半失败 | `kubemono node repair -f cluster.yaml` |
| 只改错了一个面 | `kubemono node repair -f cluster.yaml --only <phase>` |
| 不确定会改什么 | `kubemono node repair -f cluster.yaml --diff` |
| 本机有残留 etcd 数据但要重建 | `kubemono init -f cluster.yaml --force-init --allow-existing-cluster` |
| 节点改名 | `kubemono node repair -f cluster.yaml --node <新名> --rebind-identity` |
| 本机 PKI 与集群不一致（重装 master） | `kubemono join ... --replace-pki` |
| etcd 节点已损坏、无法正常退群 | `kubemono reset --leave --force-leave -y` |
| 证书快过期 | `kubemono cert check-expiration` → `cert renew` |
| 物料不完整/被改 | `kubemono bundle verify`（库或归档） |

---

## 14. 配置变更 → 该跑什么

| 改动 | 命令 | 是否可原地 |
|---|---|---|
| `cluster.hosts[].labels` / `taints` | `node repair`（该节点） | 是 |
| `node.kubelet.*`（配置/`extraArgs`/`rootDir`） | `node repair`（该节点） | 是（`rootDir` 需迁移数据） |
| `node.host.*`（内核模块/sysctl） | `node repair` | 是 |
| `node.runtime.containerd.registries` / `configPatch` | `node repair --only runtime` | 是（certs.d 变化热加载，config.toml 变化重启 containerd） |
| `node.runtime.containerd.root` | 先迁移数据，再 `node repair` | 需人工迁移 |
| `node.runtime.custom.*` | `node repair` | 是；但 containerd ↔ custom **不能**原地切换 |
| `controlPlane.apiServer/controllerManager/scheduler.extraArgs` | `node repair`（master） | 是（静态 Pod 重启） |
| `controlPlane.pki.certValidity` | `node repair`（master，certs phase） | 是（只影响后续签发） |
| `controlPlane.certSANs` / `externalEndpoints` | `node repair`（master） | 是（重签 + 重启） |
| `network.kubeRouter.*` / `kubeProxy.*` | `node repair`（master） | 是 |
| `network.plugin` | —— | **否**，只能新建集群 |
| `network.podCIDR` / `serviceCIDR` | —— | **否**，只能新建集群 |
| `addons.coredns.*` | `node repair`（master） | 是 |
| `images.components` / `componentVersions` | 重新准备物料 → `bundle load` → `node repair` | 是 |
| `kubernetesVersion` | —— | **否**，只能新建集群 + 迁移 |
| `controlPlane.etcd` local ↔ external | —— | **否**，需要迁移/重建 |
| 新增节点 | 改配置 → `validate` → 新节点 `join` | 是 |
| 删除节点 | `kubectl drain` → `reset --leave` → 从配置删除条目 | 是 |

更细的字段约束与变更边界见 [`cluster-config.md`](cluster-config.md)。

---

## 15. 常见错误信息速查

| 信息 | 出现位置 | 处理 |
|---|---|---|
| `cluster configuration ... not found: pass -f cluster.yaml` | 需要配置的命令 | 用 `-f` 指定，或把配置放到默认路径 |
| `需要 master 上的 admin kubeconfig：...` | `token create`、`reset --leave`、集群级 phase | 在 master 上执行，或用 `--kubeconfig` |
| `本机尚未安装（缺少 .../node.yaml）：请先执行 kubemono init 或 kubemono join` | `node repair` | 未安装节点不能用 repair |
| `本机已安装为节点 "x"，--node 指定了 "y"` | `node repair` | 改名迁移加 `--rebind-identity` |
| `phase "..." 不在本机修复计划中（本机可用：...）` | `node repair --only` | 用报错列出的 phase 名 |
| `--diff 与 --only 不能同时使用` | `node repair` | 分开执行 |
| `--purge ... 不能与 --keep-data/--keep-hostinit 同时使用` | `reset` | 二选一 |
| `--kubeconfig 只在 --leave 时使用` / `--force-leave 只在 --leave 时使用` | `reset` | 去掉 flag 或加 `--leave` |
| `已取消` | `reset`、`cert renew` | 加 `-y` 或交互确认 |
| `本机没有 CA 私钥 ... 无法续期` | `cert renew` | 到 master 上执行 |
| `未知证书 "..."；可用 --cert ...` | `cert renew --cert` | 用 §6.3 的 ID 列表 |
| `<path> 已存在；...本命令不覆盖已有文件` | `pki issue` | 移走文件或改 `--cn`/`--out-dir` |
| `架构 "..." 不在 BOM 的 arch 列表里` | `bundle pack` / `bundle list` | 用 BOM 支持的架构；一次一个 |
| `--offline 与 --refresh 不能同时使用` | `bundle pack` | 二选一 |
| `归档 ... 的 sha256 是 ...，期望 ...` | `bundle verify` / `bundle load` | 重新拷贝归档或更新 `.sha256` |
| `物料库里有多个 release（...），请用 --release 或 -f 指定` | `bundle verify` | 显式指定 release |
| `unsupported --output "..."` | 所有支持 `--output` 的命令 | 只接受 `table\|json\|yaml`（`config default` 只接受 `yaml\|json`） |

---

## 16. 与 `cluster-config.md` 的分工

| 想知道 | 看哪里 |
|---|---|
| 某个字段能不能写、默认值、格式、相互约束 | [`cluster-config.md`](cluster-config.md) |
| 某条命令做什么、什么时候用、参数怎么填 | 本文 |
| 改了配置后跑哪条命令、能不能原地改 | 本文 §14 + `cluster-config.md` §8（可自定义项）/ §10（常见坑） |
| 安装/加入/退群/重置的完整流程 | 本文 §13（更细的运行时/网络说明见 `cluster-config.md` 对应章节） |
| 证书怎么续、怎么查、怎么签 | 本文 §6、§7 |
| 物料怎么造、怎么导入、怎么校验 | 本文 §8 |

两篇文档配合使用的典型路径：

```text
config default → 编辑 cluster.yaml（对照 cluster-config.md）
→ config validate → bundle load → init / join
→ 日常只改 cluster.yaml（对照 §14 决定命令）→ node repair
→ 证书到期看 §6，节点下线看 §10
```
