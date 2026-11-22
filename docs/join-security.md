# Kubernetes 节点加入（join）安全设计

> **范围**：讨论 master / worker 节点加入集群时的信任建立与凭证分发，不绑定任何具体发行版或安装器。
> 第 1–12 节与实现无关；第 13 节把结论映射到 kubemono 的当前实现与演进方向。
>
> **要解决的问题**：一旦 join 凭证被复制到磁盘、备份、聊天记录或 CI 日志，删除副本就不是
> 安全控制。正确的目标是让凭证本身短期、可失效，而不是依赖操作者“记得删除”。
>
> **当前实现**：采用短期凭证模型。`init` 只建集群，不生成 join 物料；
> `kubemono token create` 按角色签发（默认 worker；master 必须显式 `--role master`，
> 同时轮换 certificateKey 并刷新证书包）。join 命令携带
> `--token` + `--discovery-token-ca-cert-hash` + `--control-plane`/`--certificate-key`，
> 不落盘；bootstrap token 的 TTL 硬上限为 2 小时。第 5–12 节的 L3/L4、票据、审批等
> 属于后续演进方向，不在当前实现中。

---

## 0. 一页纸结论

### 0.1 十条结论

1. **join 不是装软件，是授权一台机器持有集群的根信任。** join 文件不是配置文件，是一张“持有即有权”的
   能力凭证（capability）。capability 必须有 subject、scope、expiry、usage、revocation，而 YAML 文件天生没有
   后三项——除非服务端强制。
2. **master 文件和 worker 文件的危险等级差一个数量级。** master 文件解出来的是 CA 私钥、SA 私钥、
   etcd CA 私钥和 etcd 静态加密密钥；worker 文件是一张可以在有效期内加入任意数量节点的通行证。
3. **“用后立即删除”是流程，不是控制。** 它不可验证、不可审计、无法覆盖副本；备份、快照、聊天记录、
   CI 日志、跳板机、每台被加入节点的 `/root` 都会留下副本。删除依赖人的记忆，而人的记忆不是安全边界。
4. **正确的目标不是“文件不存在”，而是“文件存在也无效”。** 四个手段：分钟级 TTL、一次性、绑定节点、
   服务端吊销。删除只是纵深防御的最后一道，不能当主控制。
5. **用户不删文件的真正原因不是懒，而是删了就没法扩容。** 文件是集群的“备用钥匙”；只要扩容依赖它，
   它就会被留下。解法是让扩容不依赖文件——按需签发。
6. **24h + 可复用 + 多节点共享，把“加入集群”变成了一项持续 24 小时、无归属、可无限复制的权限。**
   这不是临时凭证，这是“看起来临时的长期凭证”。
7. **CSR 自动审批 + 无节点绑定，使任何拿到令牌的人可以为任意节点名申请证书。** 客户端的 role 校验
   只防手滑，不防攻击者；安全边界必须在服务端。
8. **master 与 worker 必须分级。** worker：一次一票、自动审批、只给 CA 证书；master：一次一票、
   机器身份 + 人工/双人审批、信封加密、用后即焚。
9. **长期方向是“无票据”。** 云上用实例身份（IAM / IMDS / identity token），自建机房用 TPM 证明 + 清单；
   票据只是没有硬件根时的过渡方案。
10. **验收标准只有一条：** 把 join 文件留在磁盘上 30 天，再拿来用，必须失败；把它用在第二台机器上，
    必须失败；用它申请 master 物料，必须失败。

### 0.2 如果只做三件事

1. **把凭证寿命从 24h 降到 10 分钟，并且一次性。** 由服务端消费，不靠客户端删除。
2. **把凭证绑定到“这一台节点、这一个角色”。** 服务端校验 CSR 的 CN 与票据一致；不在清单里的节点名直接拒绝。
3. **默认不落盘。** 命令行只输出一次凭证；需要文件时由调用方显式导出，并带上到期时间。

> 这三件事做完，即使 join 文件被复制到聊天记录、备份和每台节点上，它也已经是一张过期彩票。

---

## 1. 问题重述：join 到底是什么

### 1.1 join 做的三件事

一台新机器加入集群，本质上要完成三件事：

| # | 事情 | 方向 | 产物 |
|---|---|---|---|
| 1 | 新节点信任集群 | 节点 → 集群 | 集群 CA 证书 / CA pin |
| 2 | 集群信任新节点 | 集群 → 节点 | kubelet 客户端证书（`system:node:<name>`） |
| 3 | 新节点获得运行所需物料 | 集群 → 节点 | worker：无；master：CA/SA/etcd/加密密钥包 |

第 1 步是“验证服务端”，第 2 步是“服务端验证客户端”，第 3 步是“分发根信任”。三者都是**信任建立**，
不是文件拷贝。把 join 当成“把 YAML 拷过去执行一下”，是后面所有问题的起点。

### 1.2 join 物料是能力凭证，不是配置文件

安全工程里，capability 是“持有即有权”的凭证。一张合格的 capability 需要五个要素：

| 要素 | 含义 | 典型 join 文件里有吗 |
|---|---|---|
| Issuer | 谁签发的 | 隐含在集群里，文件本身不体现 |
| Subject | 给谁的 | 没有，谁拿到都能用 |
| Scope | 能干什么 | 只有角色，没有节点、没有次数 |
| Expiry | 什么时候失效 | 有（常见 24h），但太长且可复用 |
| Usage / Revocation | 用几次、怎么撤销 | 无次数限制；只能等过期 |

缺了 Subject、Usage、Revocation，文件就退化成“一张谁捡到都能用的门禁卡”。YAML 文件天然不会被服务端
“消费”，所以**只有服务端状态才能补上这三个要素**。这就是为什么本文反复强调：修复点在服务端，
不在文件权限、不在 README 的警告、也不在 `shred`。

### 1.3 master 与 worker 的不对称

| 维度 | worker join | master join |
|---|---|---|
| 节点拿到什么 | 集群 CA 证书 + 自己的 kubelet 证书 | CA 私钥、SA 私钥、etcd CA 私钥、静态加密密钥、front-proxy CA 私钥 |
| 最坏后果 | 伪节点占资源、读本节点 Pod 的 Secret、作为横向移动跳板 | 伪造任意身份、解密 etcd 备份、永久接管集群 |
| 可逆性 | 较高：删 Node、短证书、网络隔离 | 很低：根 CA 泄露后只能轮换根、重建集群 |
| 审批强度 | 自动 + 绑定即可 | 人工/双人 + 机器身份 + 信封加密 |
| 凭证寿命 | 10 分钟量级 | 5–10 分钟量级，且严格一次性 |

**结论：两者不能共用同一种凭证形态、同一种寿命、同一种审批。** 一个 24h、可复用、角色字段可被客户端
校验的共享文件，对 worker 已经偏松，对 master 则是不可接受的。

### 1.4 主流做法的共同假设

k3s、RKE2、k0s、Talos 以及大多数自建安装器，在 join 上共享同一组假设：

1. join 物料只在“安装窗口”存在；
2. 操作者会在用完后删除它；
3. 传输介质（scp / U 盘 / 聊天工具）是可信的；
4. 被加入的节点在加入前是可信的；
5. 一个共享令牌足以表达“哪台机器、什么角色、加入几次”；
6. 24 小时是很短的时间。

这六条在实验室里成立，在生产里一条都不成立。本文后面的设计，就是逐条拆掉这些假设。

---

## 2. 威胁模型

### 2.1 资产

| 资产 | 位置 | 泄露后果 | 有效期 |
|---|---|---|---|
| bootstrap token（worker） | join 文件、`kube-system/bootstrap-token-*` | 可加入任意数量伪节点（如果无绑定） | 默认 24h |
| bootstrap token（master） | join 文件、同上 | 可读证书包；可加入伪节点 | 默认 24h |
| certificateKey | master join 凭证 | 解密证书包；单独拿到无密文则无用 | 直到下次签发 master 凭证 |
| 证书包密文 | `kube-system/kubemono-certs` | 与 key 组合后泄露根信任 | 长期存在（通常无 TTL） |
| 证书包明文 | 解压后落到 master 的 `/etc/kubernetes/pki` | 同上 | 永久 |
| CA 证书 / pin | join 文件 | 公开信息，泄露不致命 | — |
| clusterID / endpoints | join 文件 | 低敏感，可用于侦察 | — |
| 加入后的 kubelet 证书 | 节点 | 节点身份；可读本节点 Pod 的 Secret | 默认 1 年 |

真正需要保护的是前四项。后两项经常被误认为“也是秘密”，导致注意力放错地方。

### 2.2 对手

| 对手 | 能力 | 典型入口 |
|---|---|---|
| 外部攻击者 | 拿到文件副本，但不在内网 | 公开仓库、CI 日志、聊天记录、镜像层 |
| 内部人员 | 有内网访问、有跳板机、可能读过文档 | 共享运维机、离职交接、越权 |
| 被攻陷的节点 | 已加入集群，有节点身份 | 节点上的残留文件、节点 root |
| 供应链 | 控制镜像、配置管理、CI | 镜像层、Terraform state、Ansible vars |
| 物理对手 | 拿到磁盘、备份、旧笔记本 | 退役设备、备份磁带、快照 |
| 意外 | 没有恶意，只是复制/截图/贴日志 | 截图、录屏、工单 |

### 2.3 攻击树

```text
拿到 join 文件
├─ 在有效期内直接 join
│  ├─ worker 文件 → 伪节点 → 读本节点 Secret / 占资源 / 打控制面网络
│  └─ master 文件 → 读 kubemono-certs → 解出 CA/SA/etcd/加密密钥 → 永久接管
├─ 复制到多台机器 → 批量伪节点（无绑定、无次数限制时）
├─ 改 role / 改字段重写 YAML（客户端校验不构成边界）
├─ 用 worker 令牌尝试读证书包（应被 RBAC 拒绝）
├─ 冒用已有节点名申请证书（CSR 无绑定时的 DoS / 混淆）
├─ 重放（同一票据第二次使用）
└─ 等文件“过期”后再用（若 TTL 长、被续期、或从备份恢复）
```

还有一条不在文件里、但同样重要的路径：

```text
中间人 / 假 apiserver
├─ 骗取引导令牌（如果首跳不校验服务端）
├─ 让节点加入假集群（写入假 CA）
└─ 把 join 流量重放到真集群
```

### 2.4 泄漏渠道

| 渠道 | 能删吗 | TTL 还有效吗 |
|---|---|---|
| 管理员工作站 `/root`、`/tmp`、home | 也许 | 有效 |
| 每台被加入节点的 `/root`、`/tmp`、scp 目标 | 通常忘 | 有效 |
| 跳板机 / 共享运维机 | 很难 | 有效 |
| Shell history、tmux/screen 回滚 | 很难 | 有效 |
| CI 日志、`set -x`、构建产物 | 很难 | 有效 |
| 聊天工具、邮件、工单、Wiki | 不能 | 有效 |
| Git 仓库（含私有）、Terraform state、Ansible vars | 不能（历史） | 有效 |
| 镜像层、云主机快照/AMI、备份系统、Time Machine | 不能 | 有效 |
| 故障转储、journald、审计日志 | 不能 | 有效 |
| 离职员工的笔记本 | 不能 | 有效 |

**能删的只是少数；唯一全局有效的是让凭证过期/一次性。** 这张表是本文最重要的论据：
你不可能赢一场“把所有副本都删掉”的战争，但你可以让所有副本在同一时刻一起失效。

### 2.5 风险矩阵

| 方案 | 泄漏可能性 | master 泄漏影响 | worker 泄漏影响 | 综合 |
|---|---|---|---|---|
| L0 长期共享 token | 高 | 灾难 | 高 | 不可接受 |
| L1 24h token + 文件 | 高 | 灾难（24h 窗口） | 中高（可加任意多节点） | 仅实验室 |
| L2 短 TTL + 一次性 | 中 | 高（窗口内仍可接管） | 低 | 可用于非生产 |
| L3 + 节点/机器绑定 + 审批 | 中 | 中（需同时攻破机器身份/审批） | 低 | 生产可用 |
| L4 无票据（身份证明） | 低 | 低 | 低 | 高安全目标 |

---

## 3. “用完删除”为什么不是安全控制

### 3.1 不可验证

安全控制要满足三条：可执行、可验证、可审计。删除一条都不满足：

- **可执行**：谁来删？什么时候算“用完”？加完所有节点？那永远没到。
- **可验证**：你怎么证明备份里、快照里、聊天记录里、每台节点的 `/tmp` 里都没有？
- **可审计**：服务端怎么知道文件还在不在？它连文件是否存在都不知道。

一个无法验证的控制，在审计和事故复盘里等于不存在。

### 3.2 用户不删是理性的

把“用户不删”归因为粗心，会错过真正的设计问题。真实动机至少有五种：

1. **一次性安装后忘了。** 装完集群，注意力立刻转移到 CNI、存储、监控；join 文件不在任何 checklist 上。
2. **留着以备扩容。** 这是最常见、也最理性的原因：删了就得重新导出，还要记得命令、找到 admin kubeconfig。
   **文件是集群的备用钥匙，而备用钥匙当然要留着。**
3. **当作 DR 备份。** “万一 master 挂了，还能用它重建。”这是错误但普遍的直觉；join 文件不是备份，
   它既可能已过期，也可能在新一次导出后失效。
4. **团队共享。** 放到跳板机或共享目录，谁装节点谁拿。于是文件从“一个人的临时文件”变成“团队的基础设施”。
5. **进入自动化。** 写进 Ansible vars、Terraform state、镜像、CI secret，从此有了几十个副本和一条永久历史。

**当安全动作比不安全动作更麻烦时，用户会选不安全的。** 正确的修复不是加大警告字号，而是让“不保留文件”
成为更省事的那条路：按需签发、用完即弃、扩容随时可以再要一张票。

### 3.3 删除不等于消失

- `unlink` 只是去掉目录项；SSD 的 wear leveling、CoW 文件系统、日志型文件系统都可能保留数据块。
- 备份、快照、Time Machine、云盘同步、IDE 本地历史、编辑器 swap 文件，都是副本。
- `shred` 在 SSD/CoW/网络文件系统上不保证有效。
- 你把文件从 A 删了，B 上的副本还在；你甚至不知道 B 存在。

所以“删除”最多是“降低副本数量”，不是“消除泄露”。

### 3.4 备份与合规让删除更难

- 备份保留策略通常要求 30/90/365 天；你不可能为了一个 join 文件去删整个备份。
- 审计、工单、CI 日志往往要求留存；join 内容可能已经在里面。
- 事故复盘时，你无法证明“那个副本删了”；但你可以证明“那个令牌在 10 分钟后就失效了”。

**这就是 TTL 的价值：它是唯一能穿透备份、快照和历史记录的控制。** 备份里躺着的是 10 分钟前就死掉的票据，
而不是一把永久钥匙。

### 3.5 结论：假设已泄漏

把 join 物料当成“已经泄露”来设计。安全属性从

> “文件会被删除，所以不会泄露”

改成

> “文件即使被保留、被复制、被备份，也已经无效”。

这不是悲观，而是把控制点从人转移到系统。

---

## 4. 安全目标：九条不变量

任何 join 设计（不论用什么工具）都应该满足下面九条。它们就是后文所有方案的评分标准。

| # | 不变量 | 说明 | 违反后果 |
|---|---|---|---|
| I1 | **时限** | 可用于 join 的凭证 ≤ 15 分钟（master ≤ 10），以服务端时钟为准 | 长期暴露 |
| I2 | **一次性** | 成功即消费；失败 N 次或 TTL 到期也消费 | 重放、批量伪节点 |
| I3 | **绑定** | clusterID + role + node 名（+ 机器身份/IP）由服务端校验 | 冒名、跨节点复用 |
| I4 | **默认无文件** | 凭证经 TTY/stdin 传递；落盘是显式选择且带到期时间 | 副本泛滥 |
| I5 | **用后即焚** | 成功加入后令牌/票据/证书包在服务端被删除，并记审计事件 | 长期可组合利用 |
| I6 | **分级** | worker 票不能读 master 物料；master 票不能当 worker 票复用 | 权限提升 |
| I7 | **可归因** | 每次签发/使用/失败都有 actor、node、IP、ticket id | 无法调查 |
| I8 | **端到端加密** | master 物料只对目标机器的临时公钥加密 | 被动窃取、日志/etcd 泄露 |
| I9 | **可检测** | 票据有可识别前缀/校验和；异常使用可告警 | 泄露后无人知晓 |

补充两条工程约束：

- **I10 可离线但不长期**：离线/air-gap 场景允许离线签发票据，但有效期 ≤ 1h，且节点绑定。
- **I11 失败要可恢复**：票据过期必须能一条命令重发，不能要求重建集群；否则用户又会回到“留一把备用钥匙”。

---

## 5. 设计空间：八个维度

### 5.1 维度总表

| 维度 | 弱 → 强 | 代价 | 关键权衡 |
|---|---|---|---|
| 寿命 | 永久 → 24h → 1h → 10min → 一次性 → 无凭证 | 需要按需签发 | 便利性 vs 暴露窗口 |
| 使用次数 | 无限 → N 次 → 1 次 | 需要服务端消费 | 复用 vs 可归因 |
| 绑定 | 无 → 集群 → 角色 → 节点名 → 清单+IP → 机器身份 → 人 | 需要清单/证明 | 兼容 NAT/动态 IP |
| 授权点 | 客户端校验 → RBAC → CSR approver → 策略引擎 → 双人 | 需要控制器 | 复杂度 vs 强度 |
| 分发 | 文件 → stdout → stdin/TTY → secret manager → 无 | 需要交互/集成 | 自动化友好度 |
| 封装 | 明文 → 共享 key → 每票信封 → 每机密钥（TPM） | 需要密钥协商 | 实现复杂度 |
| 吊销 | 等过期 → 手工删 → 自动消费 → 持续证明/短证书 | 需要 GC | 运维负担 |
| 存储 | 明文 etcd → 哈希票据 → 签名票据+nonce → 无状态身份 | 需要认证器 | 兼容性 |

### 5.2 逐维取舍

**寿命。** 24h 不是“短”，而是“覆盖整个 rollout”。一个 100 节点的安装可能持续数天，文件在 init 时生成，
在最后一天仍然有效。寿命应该由“这次加入需要多久”决定，而不是由“建集群的时间”决定。
按需签发后，10 分钟对单节点足够；批量 30 分钟；离线 1 小时封顶。

**使用次数。** 无限次使用意味着无法归因：日志里只有 `system:bootstrap:abc123`，你不知道是哪台机器、
第几次。一次性使用让“一张票 = 一台机器 = 一次加入”，审计才有意义。

**绑定。** 绑定越强，泄漏后的可利用性越低。注意绑定强度是有层级的：
节点名（可被 `--hostname-override` 伪造）< IP（NAT/多网卡会误伤）< machine-id（节点 root 可改）<
TPM EK/AK 证明（硬件根）≈ 云实例身份（云厂商签名）。**只有后两者是真正的边界，前两者是减速带。**

**授权点。** 客户端校验（“这个文件是 master 的”）只防手滑；RBAC 能防越权读，但防不了“用合法令牌加入
任意节点”；CSR approver 才是“谁能以什么身份加入”的决策点。**没有 approver 的 join，等于没有授权。**

**分发。** 文件是最差的分发方式，因为它在磁盘上留下了可复制的副本；stdout 次之（会进 CI 日志/回滚缓冲区）；
stdin/TTY 较好（不落盘、不进 history）；secret manager 适合自动化；身份证明最好（没有可分发的秘密）。

**封装。** 明文传输（http）不可接受；共享 key（一个 certificateKey 给所有 master）把“两把钥匙”变成
“一把钥匙”；每票信封加密（X25519）让响应只对目标机器的临时私钥可解；TPM sealed key 更进一步，
但需要硬件支持。

**吊销。** 过期是唯一不需要人操作的吊销。手工删 token 依赖记得；自动消费需要控制器；短证书 + 轮换
是节点证书层面的“持续吊销”。

**存储。** bootstrap token 在 etcd 里必须明文（上游 authenticator 的需要），只能靠 etcd 加密 + 最小 RBAC；
自建票据可以只存哈希；签名票据（HMAC/Ed25519）可以做到“服务端不存票据本身，只存已用 nonce”。

### 5.3 组合出来的方案谱系

把上面的维度组合，得到从弱到强的谱系：

```text
L0  长期共享 token（无 TTL / 很长 TTL，文件分发）
L1  TTL token + CA pin + JWS（短期凭证）
L2  短 TTL + 一次性 + 自动吊销（票据）
L3  L2 + 节点/机器绑定 + 服务端审批
L4  无票据：云实例身份 / TPM 证明 / SPIFFE
```

下一节逐级展开。

---

## 6. 方案分级 L0–L4

### 6.1 分级表

| 级别 | 凭证形态 | 寿命/次数 | 绑定 | 残留文件最坏后果 | 便利性 | 适用 |
|---|---|---|---|---|---|---|
| L0 | 长期共享 token | 永久/无限 | 无 | 永久后门 | 最高 | 不要用 |
| L1 | token + certKey 文件 | 24h/无限 | 角色 | 24h 内可接管或加任意多节点 | 高 | 实验室 |
| L2 | 一次性短票 | 10min/1 次 | 角色+集群 | 10min 内一次，且通常来不及 | 高 | 非生产/小规模 |
| L3 | 一次性短票 + 绑定 | 10min/1 次 | 节点+机器身份+审批 | 需要同时攻破机器身份/审批 | 中 | 生产 |
| L4 | 身份证明 | 无票据 | 硬件/云身份 | 需要攻破硬件/云账号 | 中（需集成） | 高安全目标 |

### 6.2 L1 的具体问题

L1 是当前大多数安装器的状态，也是 kubemono 的现状。它比 L0 好，但仍有五个结构性问题：

1. **24h 太长。** 覆盖整个 rollout，且文件通常被复制到每台节点。
2. **可复用。** 一个 worker 令牌可以在 24h 内加入任意多台机器；日志无法区分。
3. **无节点绑定。** 任何持令牌者可以为任意 CN 申请 kubelet 证书（CSR 自动审批时）。
4. **master 物料长期可解。** certificateKey 静态、与 token 同文件；证书包 Secret 通常没有 TTL。
5. **客户端校验不是边界。** “worker 文件不能有 certificateKey”只是 CLI 的礼貌，攻击者自己写 YAML。

L1 适合实验室和一次性演示；不适合任何多租户、长期运行、有人离职、有备份的环境。

### 6.3 L2 的最小可行设计

L2 不改变架构，只改变凭证的生命周期：

- 每个节点一张票，TTL 10 分钟；
- 票在服务端记录状态（issued / used / expired）；
- 加入成功后消费票据，并删除对应的 bootstrap token；
- 证书包 Secret 设置 TTL（例如 15 分钟），到期删除；
- 票据经 TTY/stdin 输入，默认不落盘。

**残留文件后果**：10 分钟后失效；如果已经被合法节点用过，立即失效；无法用于第二台机器（若加了绑定）。
这已经消除了 90% 的“忘记删文件”风险。

L2 的不足：没有节点绑定，票据在 10 分钟内仍可被用于任意节点名；master 物料仍用共享 certificateKey；
审批仍是自动的。所以 L2 适合非生产，或作为 L3 的第一步。

### 6.4 L3 的绑定与审批

L3 在 L2 之上加三件事：

1. **节点绑定**：票据记录 `node`、`role`，CSR approver 校验 `CN == system:node:<node>`；
   不在集群清单里的节点名直接拒绝。
2. **机器身份**：master 票据绑定 TPM EK / 云 instance ID / 至少 machine-id + IP；worker 可选。
3. **审批**：worker 自动（绑定通过即可）；master 人工确认，生产建议双人。

**残留文件后果**：即使票据被复制，也只能在目标机器上、用一次、10 分钟内；master 还需要审批人点头。
攻击者要成功，必须同时拿到票据、控制目标机器（或伪造其身份）、并绕过审批——这三件事同时发生的概率
远低于“捡到一个 YAML”。

### 6.5 L4 的无票据

L4 不分发“加入集群的秘密”，而是让节点**证明自己是谁**：

| 场景 | 证明方式 | 服务端校验 |
|---|---|---|
| AWS | IMDSv2 + 实例身份文档（PKCS7 签名） | 验签 + 实例 ID/标签在清单内 |
| GCP | 实例 identity token（JWT） | 验签 + audience + 实例名 |
| Azure | attested MSI token | 验签 + 资源 ID |
| 自建 + TPM 2.0 | EK 证书 + AK quote（nonce + PCR） | 验 EK 链 + quote + 清单映射 |
| 自建 + SPIFFE | 节点 SVID（SPIRE agent 证明） | 验 SVID + 信任域 |
| 离线 | 离线根签名的短票 + nonce 防重放 | 验签 + nonce 表 |

L4 的“凭证”是机器本身或云账号，没有可复制的文件。代价是需要集成（云 API、TPM、SPIRE），
以及清单/标签的维护。对于云上集群，这几乎是免费的安全提升；对于自建机房，TPM 是长期方向。

### 6.6 残留文件后果对照

这张表直接回答“如果我忘了删，会怎样”：

| 设计 | 文件被复制到聊天/备份 | 30 天后 | 用在第二台机器 | 用来拿 master 物料 |
|---|---|---|---|---|
| L0 | 永久可用 | 可用 | 可以 | 可以 |
| L1 | 24h 内可用（可多次） | token 已失效；但有效期内若被用过，接管已经发生 | 可以（24h 内） | 可以（master 文件，24h 内） |
| L2 | 10min 内可用一次 | 无效 | 可以（未绑定） | 可以（若拿到 master 票） |
| L3 | 10min 内仅限该节点一次 | 无效 | 不可以 | 需要审批 + 机器身份 |
| L4 | 无可用凭证 | 无效 | 不可以 | 不可以 |

**L3 是“忘记删文件”这个问题的正确答案**：它不要求你删，它要求你在 10 分钟内用它、只用一次、
只能在这一台机器上用。删除变成了“可选的卫生习惯”，而不是“必须的安全步骤”。

---

## 7. 专题：master join 与根信任

### 7.1 一个 master join 到底交出什么

以典型的自建控制面为例，master 加入时下发（或解密后落盘）的物料包括：

| 文件 | 作用 | 泄露后果 |
|---|---|---|
| `pki/ca.key` | 集群根 CA 私钥 | 伪造任意客户端/服务端证书，直接 `system:masters` |
| `pki/sa.key` | ServiceAccount 签名私钥 | 伪造任意 SA token，读写全部 API |
| `pki/front-proxy-ca.key` | 聚合层 CA 私钥 | 冒充聚合 apiserver，截获请求 |
| `pki/etcd/ca.key` | etcd CA 私钥 | 签发 etcd client/peer 证书，直读 etcd |
| `encryption/config.yaml` | etcd 静态加密密钥 | 解密 etcd 备份里的所有 Secret |
| `pki/ca.crt`、`pki/sa.pub` | 公钥材料 | 公开信息 |

这包东西的本质是**集群的根信任 + 数据-at-rest 密钥**，不是“证书”。拿到它的人不需要再“加入集群”，
他可以直接造一个集群，或者把现有集群的所有秘密读光。

> 因此：**master join 的安全等级 = 根 CA 备份的安全等级 = 集群的最高安全等级。**
> 任何把 master join 当成“再装一台机器”的设计，都是错的。

### 7.2 四种 CA 托管模型

| 模型 | 做法 | 单 master 失陷后果 | 复杂度 | 适用 |
|---|---|---|---|---|
| M1 复制根 CA | 每个 master 都拿 `ca.key`（kubemono 现状） | 根 CA 泄露，集群永久沦陷 | 低 | 一般生产 |
| M2 每票信封加密 | 根 CA 仍下发，但只对目标机器临时公钥加密 | 被动窃取失效；票据被盗仍可 | 中 | 推荐的第一步 |
| M3 每 master 中间 CA | 根 CA 离线；每个 master 拿一张中间 CA 证书+私钥 | 只泄露一个中间 CA，可吊销/轮换 | 高 | 高安全 |
| M4 外部签名 | 根 CA 在 HSM/KMS，由自定义 signer/controller 代签 | 根不出硬件 | 最高 | 金融/合规 |

说明：

- **M1 不是“错”，而是“默认”**。它把根 CA 复制到每台 master，换来了简单和 HA；代价是任何一台 master
  失陷都等于根失陷。
- **M2 解决“传输/存储被被动窃取”**，不解决“票据被偷后攻击者自己发起加入”。所以 M2 必须和 L3 的
  绑定/审批一起用。
- **M3 是自建机房性价比最高的增强**：根 CA 离线保存，每个 master 用中间 CA；kube-controller-manager
  的 `--cluster-signing-cert-file/key-file` 指向中间 CA；中间 CA 泄露只影响一个 master，可以换发。
  代价是信任链、轮换、以及 etcd/apiserver 证书都要能链到根。
- **M4 在 Kubernetes 里没有原生支持**：KMS v2 只做 etcd 加密，不做 CA 签名。要做 M4，需要自定义
  signer/controller 或外部 CA 服务。云上可以用云 KMS 的非对称签名能力自己搭。

### 7.3 certificateKey 的五个结构性问题

kubemono 当前用一个 `certificateKey` 加密证书包，再由 master 加入者解密。
它不是“错”，但有五个结构性问题：

1. **静态。** 一把 key 用到底，直到下一次 `kubemono token create --role master`；期间所有 master 加入共享它。
2. **同处输出。** token 和 key 出现在同一条 join 命令里，“读密文的凭证”和“解密文的钥匙”变成一件东西。
   两因素认证退化成一因素。
3. **共享。** 所有 master 加入者用同一把 key；无法知道“是谁解密的”。
4. **密文长期存在。** 证书包 Secret 通常没有 TTL；key 的有效期等于密文的生命周期。
5. **无归因。** 读取 Secret 的审计只能看到 `system:bootstrap:<id>`，而 id 是共享票据的一部分。

**改进方向**：把“共享静态 key”换成“每票一次性的密钥协商”。见 7.4。

### 7.4 信封加密流程

目标：证书包只对**这一台**加入机器的**这一次**加入可解；服务端不保存明文，也不保存可复用的解密 key。

```text
Joiner                                          Broker
  |                                                |
  |-- (1) 生成临时 X25519 密钥对 (ephJ_pub, ephJ_priv)
  |-- (2) 提交 ticket + node + ephJ_pub + 机器证明 -->|
  |                                                |-- (3) 验票、验绑定、验审批
  |                                                |-- (4) 生成临时 X25519 (ephB_pub, ephB_priv)
  |                                                |-- (5) shared = ECDH(ephB_priv, ephJ_pub)
  |                                                |       key = HKDF(shared, transcript)
  |                                                |       ct  = AEAD(bundle, key)
  |<-- (6) ephB_pub + nonce + ct ------------------|
  |-- (7) shared = ECDH(ephJ_priv, ephB_pub)
  |-- (8) 解密、写 PKI、启动控制面
  |-- (9) 删除响应、销毁临时私钥
  |                                                |-- (10) 消费票据、删除响应、审计
```

关键点：

- **前向保密**：双方都用临时密钥；临时私钥销毁后，即使响应被录音/备份，也无法事后解密。
- **服务端不落地明文**：broker 只在内存里组装密文，响应 Secret 用后删除。
- **一次性**：票据消费后，响应 Secret 删除；即使攻击者拿到旧响应，也没有私钥。
- **可审计**：broker 记录“谁在什么时候为哪张票交付了证书包”。

如果暂时做不到信封加密，退而求其次：

- 每次导出轮换 certificateKey（kubemono 已做）；
- 证书包 Secret 设置 TTL（例如 15 分钟）并自动删除；
- 只允许 master 组读取，且该组只绑这一条 RBAC；
- 导出后立即提示“这是一次性物料”，但仍按 L3 的票据模型管理。

### 7.5 加入后的验证与根泄露处置

master 加入后，至少做四件事：

1. **核对身份**：新 master 的 apiserver 证书指纹、Node 对象、etcd member ID 是否与预期一致。
2. **核对成员**：`etcdctl member list` 是否只多了预期的一台；quorum 是否健康。
3. **核对审计**：谁创建了票、谁批准、从哪个 IP 加入、用了哪张票。
4. **核对组件**：kube-system 下的控制面组件是否都用了新证书，是否有异常 `system:masters` 证书。

另外，**master join 应该串行化**：一次只加一台，确认 quorum 健康后再加下一台。并发加入多台 master
既会把 etcd quorum 置于风险中，也让“哪台机器在什么时候加入”的审计变得难以归因。

如果怀疑 master 物料泄露：

- **不要只删文件。** 文件只是副本，密钥已经泄露。
- 评估是否轮换根 CA（重签所有证书，代价高）、SA key（所有 token 失效）、etcd 加密 key（重加密所有 Secret）。
- 如果用了中间 CA（M3），吊销/轮换对应中间 CA，影响面小得多。
- 审计所有 `certificatesigningrequests`、`nodes`、etcd member 变更；检查是否有未知证书。
- 把这次事件写进 playbook：**根 CA 泄露的响应不是“删文件”，是“重建信任”。**

---

## 8. 专题：CSR 审批与节点身份

### 8.1 自动审批的缺口

Kubernetes 的 kubelet 客户端证书走 TLS Bootstrap：kubelet 用 bootstrap token 创建 CSR，内置 approver
在满足条件时自动批准。典型的 RBAC 绑定是：

```yaml
# 允许引导组创建 nodeclient CSR
kind: ClusterRoleBinding
metadata:
  name: kubemono:nodeclient:autoapprove
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: system:certificates.k8s.io:certificatesigningrequests:nodeclient
subjects:
- kind: Group
  apiGroup: rbac.authorization.k8s.io
  name: system:bootstrappers:kubemono:worker
```

内置 approver 的检查大致是：CSR 的 signer 是 kubelet 客户端 signer、请求者在 `system:bootstrappers` 组、
RBAC 允许创建 `certificatesigningrequests/nodeclient`。**它不检查 CN 是否等于某台预期节点，也不检查
这台节点是否在清单里，更不检查这张票是否已经被用过。**

后果：

- 一张 worker 令牌可以加入任意多台机器；
- 可以为任意节点名申请证书（包括已存在的节点名）；
- 共享令牌让日志无法归因；
- 票据过期前，攻击者可以反复使用。

### 8.2 服务端绑定的检查清单

一个合格的 CSR approver 至少检查：

| # | 检查 | 失败动作 |
|---|---|---|
| 1 | `spec.username == system:bootstrap:<ticket-id>` | deny |
| 2 | 票据存在、未过期、未消费 | deny |
| 3 | 票据的 role 与 CSR 用途匹配 | deny |
| 4 | signer 是 kubelet 客户端 signer | deny |
| 5 | CSR 的 CN == `system:node:<ticket.node>` | deny |
| 6 | CSR 的 O 包含 `system:nodes` | deny |
| 7 | `<ticket.node>` 在集群清单内 | deny |
| 8 | 该节点尚未注册（或符合 rejoin 策略） | deny |
| 9 | 来源 IP 在允许集合（可选） | deny/告警 |
| 10 | 每票尝试次数未超限 | deny + 消费票据 |

通过后：

- 给 CSR 打 `Approved` 条件；
- 标记票据 `used`，记录 CSR 名、证书指纹；
- 删除 bootstrap token Secret（消费）；
- 发 Event / 审计事件；
- 如果这是最后一张 master 票，删除证书包 Secret。

**消费时机**：消费太早（验票通过就删）会让网络失败无法重试；消费太晚（Node 注册后才删）会留下重放窗口。
推荐在“证书已签发、响应已交付”之后消费，并允许同一票据在 1 分钟内、同一节点、同一临时公钥下幂等重试一次；
重试不签发新证书。超过窗口或换节点/换公钥，一律拒绝。

拒绝后：

- 给 CSR 打 `Denied` 条件；
- 记录原因；
- 连续拒绝触发告警（可能是票据泄漏或配置错误）。

### 8.3 为什么 VAP 不够

有人会想用 `ValidatingAdmissionPolicy`（CEL）来替代 approver。它适合做**节点准入**（例如拒绝不在清单里的
Node 对象），但不能替代 CSR 绑定：

- CEL 不能解析 `spec.request` 里的 DER/PKCS#10，拿不到 CN；
- VAP 不能查询“这张票是否已用过”；
- VAP 不能原子地消费票据。

所以合理的组合是：

- **CSR approver**（小控制器）做“谁能以什么身份加入”的决策；
- **VAP / Node 准入**做第二道闸：即使证书被签发，Node 对象也只能是清单里的名字；
- **NodeRestriction**保证 kubelet 只能操作自己的 Node 对象。

三层叠加，才接近“服务端强制”。

### 8.4 重放、冒名、DoS

| 攻击 | 没有绑定时 | 有 L3 绑定时 |
|---|---|---|
| 重放票据 | 可以反复加入 | 一次性，第二次拒绝 |
| 冒用节点名 | 可以申请同名证书 | CN 必须等于票据节点；已有 Node 拒绝 |
| 批量伪节点 | 一张票加 N 台 | 一票一节点 |
| 抢注目标节点名 | 可以先注册，让真节点失败 | 机器身份/审批 + 已有 Node 检查 |
| CSR 洪水 | 限速前可打爆 approver | 每票 5 次、每 IP 20/min，超限消费 |
| 票据暴力猜解 | 取决于熵 | 128-bit + 限速，不可行 |

注意：**节点名可以被 `--hostname-override` 伪造**，所以“CN 绑定节点名”只防误用和低成本攻击，
不防有 root 的攻击者。真正的边界是机器身份（TPM/云）或人工审批。

### 8.5 续期与 rejoin

- kubelet 客户端证书续期走 `selfnodeclient`：已有 `system:node:<name>` 身份的 kubelet 可以为自己续期，
  不需要 join 票据。确保开启 `--rotate-certificates`，并设置较短的证书 TTL（例如 30 天）。
- 证书过期后的 rejoin：需要新票据；approver 允许“同名节点 + 机器身份匹配”或“管理员批准”。
- 节点退役：删除 Node、撤销 RBAC、网络隔离；Kubernetes 没有 CRL，证书在到期前仍然有效，
  所以**短 TTL + 轮换**是节点证书层面唯一的“吊销”。

---

## 9. 专题：首次信任与中间人

### 9.1 三种锚

节点第一次连 apiserver 时，必须回答“我怎么知道这是真集群”。三种锚：

| 锚 | 做法 | 防什么 | 不防什么 |
|---|---|---|---|
| CA PEM | join 文件里带 CA 证书 | 网络 MITM | 文件被偷（CA 是公开的） |
| CA hash pin | join 文件里带 CA 公钥指纹 | 网络 MITM，且不依赖证书解析 | 同上 |
| JWS | cluster-info 由令牌 secret 签名 | 伪造 cluster-info | 令牌本身被偷后，攻击者可伪造 |

三者可以叠加。当前主流做法是“CA pin + JWS”：先校验服务端证书链，再校验 cluster-info 的签名。
这能防网络层的假 apiserver，但不能防“文件被偷 + 攻击者自己当客户端”。

### 9.2 票据里该放什么

| 字段 | 放不放 | 理由 |
|---|---|---|
| clusterID | 放 | 防止加入错集群；公开信息 |
| CA PEM / hash | 放 | 首跳校验；公开信息 |
| endpoints | 放 | 发现；公开信息 |
| role | 放 | 客户端提前失败；服务端仍要校验 |
| node | 放 | 客户端提示；服务端必须校验 |
| token / ticket | **不放**（交互输入） | 避免落盘 |
| certificateKey | **不放**（信封加密） | 避免静态共享密钥 |
| 到期时间 | 放 | 客户端快速失败；服务端为准 |

核心原则：**票据文件里放“指针和证明”，不放“秘密”。** 如果必须放秘密，它必须是一次性的、分钟级的。

更彻底的做法是把现在的 join 文件拆成两份：

- 公开的 join profile：clusterID、CA、endpoints、role、node——全是公开信息，**可以随便留**；
- 一次性 ticket：只存在于终端/内存，**不落盘**。

这样即使 profile 在磁盘上躺了三年、进了备份和 Git，也不构成任何权限。**“忘记删文件”这个问题，
从根上被消解了：已经没有值得删的文件。**

但 profile 是公开的，不等于可以随便改：它决定“连谁、信谁”。如果攻击者能改节点上的 endpoints/CA，
就能把 join 引到假 apiserver 骗取 ticket。给 profile 加集群签名（验签公钥内置在二进制，或带外核对指纹），
或者用带外 CA 指纹兜底。注意：把 profile 哈希写进 ticket 只能让 broker 事后发现不一致，
不能阻止 ticket 已经被假服务器收走——**完整性必须在“连接之前”建立。**

### 9.3 禁止项

- `--insecure-skip-tls-verify`：把首跳降级为明文，令牌可被中间人窃取。
- `http://` 端点：同上。
- 不校验 clusterID：可能加入一个同名但不同的集群。
- 把 token 放在 URL query、argv、环境变量里（`ps`/日志/子进程可见）。
- 在 CI 里 `set -x` 执行 join。
- 把 join 内容贴到工单/聊天里“让同事帮忙看”。

---

## 10. 专题：存储、吊销、检测

### 10.1 存储

- **bootstrap token**：etcd 里是明文（上游 authenticator 需要）。保护手段是 etcd 静态加密（最好 KMS v2）、
  最小 RBAC（只有 kube-system 的特定角色能读）、API 审计。注意：静态加密的密钥通常就在 master 的
  `encryption/config.yaml` 里，而它又在 master join 的证书包里——所以 **master join 等于同时交出
  “etcd 密文”和“解密钥匙”**。
- **自建票据**：只存 `SHA-256(salt || ticket)`，比较用常量时间；票据本身只在签发时显示一次。
- **签名票据**：服务端不存票据，只存已用 nonce（带 TTL 的集合），适合离线和无状态场景。
- **日志脱敏**：任何路径都不打印 token/certKey；错误信息只说“票据无效/过期”，不回显内容。
- **secret scanning**：票据加可识别前缀（如 `kbj_`）和校验和，放进 gitleaks/trufflehog 规则；
  CI 对 `kind: JoinSecret`、`certificateKey:` 也做扫描。

### 10.2 吊销

| 对象 | 吊销方式 | 备注 |
|---|---|---|
| 票据 | 删除/标记 revoked；TTL 到期自动失效 | 提供 `ticket revoke`，不要让人手动 kubectl |
| bootstrap token | 删除 Secret；KCM 的 tokencleaner 清理过期 | 过期由 apiserver 拒绝 |
| 证书包 Secret | 消费后删除 / TTL 删除 | 不要让密文永久存在 |
| 节点证书 | 无 CRL；删 Node + 短 TTL + 轮换 + 网络隔离 | 这是 Kubernetes 的已知限制 |
| 根 CA | 只能轮换根、重签所有证书 | 代价极高，所以更要防泄露 |

**过期是唯一不需要人操作的吊销。** 所有“需要某人记得去删”的吊销，最终都会失败。

### 10.3 检测与蜜罐

- **审计事件**：见附录 B；至少覆盖签发、领取、批准、拒绝、消费、重放、节点注册。
- **告警规则**：
  - 同一票据第二次使用；
  - 票据在非目标节点/IP 上使用；
  - master 证书包在非预期时间被读取；
  - CSR 拒绝率突增；
  - 非清单节点注册；
  - 非工作时间的 master join。
- **蜜罐票据**：在管理机的常见路径放一份假的 master join 凭证，里面的令牌是**已注册但零权限、被标记**的
  蜜罐令牌。任何认证尝试都会以 `system:bootstrap:<id>` 出现在审计里，触发高优告警——这能抓到“翻旧文件”的
  内部人员和自动化脚本。
- **CA 指纹监控**：定期核对集群内 apiserver/etcd 证书的签发者与指纹，发现未知证书。
- **etcd 成员变更**：master join 会改 etcd membership，单独审计。

### 10.4 事件响应（playbook 摘要）

**worker 票据泄露：**

1. 删除 token / revoke 票据；
2. 查审计：是否有未知节点注册、未知 CSR；
3. 删除伪节点、隔离网络；
4. 评估是否需要轮换节点证书（短 TTL 下影响有限）。

**master 票据/证书包泄露：**

1. 假定根 CA 已泄露；
2. 若用中间 CA：吊销/轮换对应中间 CA；
3. 若用根 CA：计划根 CA 轮换（重签所有证书），轮换 SA key，重加密 etcd Secret；
4. 审计所有证书、etcd 成员、`system:masters` 绑定；
5. 复盘：票据为什么会被复制/保留？把对应控制补上。

---

## 11. 便利性设计

安全方案失败的最常见原因不是强度不够，而是“太麻烦，所以被绕过”。本节把便利性当成一等需求。

### 11.1 三条原则

1. **安全路径必须是默认路径，而且更省事。** 如果“留文件”比“重新签发”方便，用户就会留文件。
2. **按需签发，不在建集群时预制未来节点的凭证。** `init` 时写一份 24h 文件，本质是在为“未知的、
   可能几个月后才加入的节点”预授权。
3. **失败信息要给出下一条命令。** “票据过期”之后应该直接告诉用户在哪台机器上执行什么命令重发，
   而不是让他去翻文档。

**为什么票据不比文件麻烦？** 把两种流程逐步摆出来：

| 步骤 | 文件模式（对照） | 票据模式 |
|---|---|---|
| 加节点前 | 找文件、scp 到节点、确认没被改 | 在 master 上一条命令拿票 |
| 加入 | 粘贴或显式提供凭证 | `join`，粘贴票 |
| 加入后 | 记得删除（通常不删） | 无需删除；10 分钟后自动失效 |
| 24h 后 | 文件过期，重新 export、再 scp | 无影响；需要时再签 |
| 批量 | 一份文件复制到 N 台 | N 张票，一条命令生成 |
| 泄漏后 | 不知道、难以单独吊销 | 单张票可 revoke，审计可查 |

文件模式看起来“少一步”，实际上把成本转移到了“保管、复制、删除、过期后重发”上；票据模式把这些
成本消掉，只留下“输入一次码”。**对单节点是打平，对批量是更省，对安全是数量级提升。**

### 11.2 场景矩阵

| 场景 | 推荐方案 | 票据寿命 | 是否落盘 | 审批 |
|---|---|---|---|---|
| 实验室 / 单机 | 交互式一次性码 | 10 min | 否 | 自动 |
| 生产 on-prem（少量节点） | 每节点票，手工输入 | 10 min | 否 | 自动/人工 |
| 生产 on-prem（批量） | 一次生成 N 张 per-node 票，Ansible 即时签发 | 30 min | 否（stdin） | 自动 + 清单 |
| 生产 master | 每机票 + 机器身份 | 5–10 min | 否 | 人工，建议双人 |
| 云上 | 实例身份（IAM/IMDS/identity token） | 无票据 | 否 | 策略 |
| 边缘 / 离线 | 离线签名票 + CA 指纹带外核对 | ≤ 60 min | 可选（U 盘） | 人工 |
| CI / 临时集群 | 短期票，secret 注入 | 10 min | CI secret | 自动 |

### 11.3 批量与自动化

**批量签发：**

```bash
# 在任一 master 上（有 admin kubeconfig）
kubemono join ticket create --role worker \
  --nodes node-01,node-02,node-03 --ttl 30m --format table
# 输出：node-01  kbj_...  10:42 到期
#       node-02  kbj_...  10:42 到期
```

**Ansible（不落盘）：**

```yaml
- name: mint join ticket
  command: kubemono join ticket create --role worker --node {{ inventory_hostname }} --ttl 10m --format value
  delegate_to: "{{ groups['masters'][0] }}"
  register: ticket
  no_log: true

- name: join
  command: kubemono join -f /etc/kubemono/cluster.yaml --ticket-stdin
  args:
    stdin: "{{ ticket.stdout }}"
  no_log: true
```

**自动扩缩容：** 不要在 user-data / 启动脚本里塞长期 token（user-data 会被云平台保存、被实例元数据暴露）。
用云实例身份，或者由 autoscaler 在启动时调用控制面签发一张 5 分钟票。

**CI：** secret 注入 + 10 分钟 TTL + `set +x`；不要把 join 内容打进构建日志或镜像层。

### 11.4 离线与低带宽

- **离线根签名票**：离线保存 Ed25519 根密钥；`ticket sign --offline` 生成
  `{clusterID, node, role, expiry, nonce}` 的签名票据；集群侧 broker 验签 + nonce 表防重放。
- **带外核对 CA 指纹**：打印 4 个词或 16 位 hex，由现场人员与 master 屏幕核对；这是唯一真正的带外锚。
- **U 盘/纸质**：票据是短票，U 盘里即使残留，过期也无效；比长期文件安全得多。
- **低带宽**：票据只有几百字节；证书包走局域网，不需要外网。

### 11.5 失败路径的 UX

| 情况 | 用户看到的 | 用户该做的 |
|---|---|---|
| 票据过期 | “票据已于 10:42 过期（现在 10:55）。在 m1 上执行：`kubemono join ticket create --node node-7 --role worker`” | 复制命令重发 |
| 票据用错节点 | “这张票是给 node-7 的，当前节点是 node-9。请为 node-9 重新签发。” | 重发 |
| 票据已使用 | “票据已于 10:40 被 node-7（10.0.0.7）使用。如果不是你，请执行 `kubemono join audit --ticket ...`” | 调查 |
| 不在清单 | “node-9 不在 cluster.yaml 的 hosts 里。先把它加入清单，或在 master 上执行 ... 临时授权。” | 改清单 |
| 没有 admin kubeconfig | “需要在已初始化的 master 上执行；当前机器没有 admin.conf。” | 换机器 |
| 时钟偏差 | “客户端时间与集群相差 7 分钟；票据有效期以服务端为准。” | 校时 |

**永远不要在错误信息里回显票据内容**；只说 id 和状态。

### 11.6 票据寿命要覆盖“最后一个使用点”，不是“输入票据的时间”

join 不是一瞬间：发现集群、取 CA、取证书包、TLS Bootstrap、启动 kubelet 可能间隔几分钟到几十分钟。
如果票据在 kubelet 发起 CSR 之前就过期，安装会在中途失败——这是“10 分钟票据”最容易踩的坑。

两种解法：

1. **把使用点前移（推荐）**：把 join 拆成两个阶段——
   `enroll`（用票换 CA / 证书 / 证书包，一次性）和 `install`（长流程，用已经拿到的证书，无需再用票）。
   票据在 enroll 结束时就消费，后续与票据无关。
2. **让票据覆盖安装窗口**：批量/慢速环境用 30–60 分钟的票，但仍然一次性、绑定节点。

推荐 1：它让票据寿命回到 10 分钟量级，同时不牺牲安装可靠性。**“票据”和“安装”解耦之后，
票据才真正可以短命。**

---

## 12. 推荐架构（框架无关）

### 12.1 组件与职责

| 组件 | 职责 | 部署位置 |
|---|---|---|
| Issuer | 签发票据：控制面 / 离线根 / 云 IdP | master / 离线 |
| Join Broker | 验票、绑定、审批、加密响应、消费、审计 | 控制面（小控制器/服务） |
| Joiner | 生成临时密钥、提交请求、解密、落盘、清理 | 新节点 |
| Policy | 清单、角色、时间、IP、审批规则 | Broker 内/外部策略引擎 |
| Audit Store | 集群外不可篡改的审计 | 外部 SIEM/日志 |

关键设计：**Broker 是唯一同时知道“票”和“节点身份”的组件**。没有它，服务端就无法把票据绑定到节点，
只能靠客户端自觉——而客户端是不可信的。

### 12.2 worker 流程

```text
admin                    broker                         joiner (node-7)
  |                        |                               |
  |-- ticket create ------>|                               |
  |   (node-7, worker,     | 存 hash(node-7, role, exp)    |
  |    10m)                |                               |
  |<-- kbj_xxx ------------|                               |
  |                        |<-- join（交互输入票）----------|
  |                        |    + ephJ_pub                 |
  |                        |-- 验票/绑定/限速 -------------|
  |                        |-- 批准 kubelet 客户端 CSR ----|
  |                        |    （CN=system:node:node-7）  |
  |                        |-- 返回 CA（公开材料）-------->|
  |                        |                               |-- 写 ca.crt
  |                        |                               |-- TLS bootstrap 拿证书
  |                        |<-- 完成 ----------------------|
  |                        |-- 消费票据、删 token、审计     |
```

worker 不需要证书包，只需要 CA 证书；这让 worker 路径的攻击面小很多。

### 12.3 master 流程

```text
admin A              admin B (approve)        broker                joiner (m2)
  |                       |                     |                      |
  |-- ticket create ----->|                     |                      |
  |   (m2, master, 5m,    |                     |                      |
  |    bind: TPM EK)      |                     |                      |
  |                       |-- approve ticket -->|                      |
  |                       |                     |<-- join + ticket ----|
  |                       |                     |    + ephJ_pub        |
  |                       |                     |    + TPM quote       |
  |                       |                     |-- 验票/验TPM/验审批 --|
  |                       |                     |-- ECDH 加密 bundle ->|
  |                       |                     |                      |-- 解密、写 PKI
  |                       |                     |                      |-- 加 etcd member
  |                       |                     |                      |-- 起控制面
  |                       |                     |<-- 完成 -------------|
  |                       |                     |-- 删响应/票/bundle   |
  |                       |                     |-- 审计 + 验证提示    |
```

master 的两个额外要求：**第二个人点头**，以及**机器身份证明**。两者都可以按组织规模裁剪，
但至少要有其一；否则“票据被偷”仍然等于“根 CA 被偷”。

### 12.4 状态机

```text
ISSUED ──claim──> CLAIMED ──approve──> APPROVED ──deliver──> DELIVERED ──consume──> CONSUMED
   │                  │                    │                     │
   ├─ expire ─────────┴────────────────────┴─────────────────────┴──> EXPIRED
   └─ revoke ───────────────────────────────────────────────────────> REVOKED
```

每次状态转换都记录：时间、actor、node、IP、ticket id、原因。**没有状态的票据不是票据，是文件。**

### 12.5 数据结构

```yaml
apiVersion: join.kubemono.io/v1
kind: JoinTicket
metadata:
  name: jt-7f3a9c
spec:
  clusterID: 5f2c...
  node: node-7
  role: worker            # worker | master
  notBefore: "2026-11-20T09:00:00Z"
  notAfter:  "2026-11-20T09:10:00Z"
  maxUses: 1
  bind:
    ip: 10.0.0.7          # 可选
    machineID: "..."      # 可选，防跨机
    tpmEK: "..."          # master 建议
  approval:
    mode: auto            # auto | manual | two-person
    by: []
status:
  phase: Issued           # Issued|Claimed|Approved|Delivered|Consumed|Expired|Revoked
  claimedBy: ""
  consumedAt: null
  csrName: ""
  fingerprint: ""
```

存储：只存 `hash(ticket)`，不存明文；`status` 用于一次性判定；过期条目由 GC 清理。

### 12.6 票据格式与存储

- **认证方式**：票据要么复用 bootstrap token（kube-apiserver 原生支持，但 Secret 在 etcd 里是明文），
  要么自建 webhook authenticator（可只存哈希，但多一个认证组件）。前者落地快，后者存储更干净；
  kubemono 已有 bootstrap token 链路，走前者是合理的最小改动。
- **格式**：`kbj_` + 26 字符 Crockford Base32（130 bit）+ 2 字符校验和。
  前缀让 secret scanner 能识别；校验和防手抄错误。
- **存储**：`SHA-256(salt || ticket)`，salt 每票随机；比较用 `subtle.ConstantTimeCompare`。
- **显示**：只在创建时显示一次；`ticket list` 只显示 id、node、到期时间，不显示内容。
- **限速**：每票 5 次尝试，每 IP 20 次/分钟；超限消费并告警。
- **审计**：见附录 B。

---

## 13. 映射到 kubemono：当前实现与演进方向

> 本节和具体实现相关。前 12 节可以作为任何安装器的设计参考。

### 13.1 当前实现

| 行为 | 位置 | 风险 |
|---|---|---|
| 默认 TTL 2h、硬上限 2h | `internal/constants/constants.go`（`DefaultBootstrapTokenTTL` / `MaxBootstrapTokenTTL`） | 窗口内仍可复用/跨节点 |
| 令牌可复用、多节点共享 | `kubemono token create` 的默认行为 | 无法归因、可批量加入 |
| join 物料按角色签发 | `internal/cli/token` / `internal/join/token` | worker-only 扩容不轮换 certificateKey |
| master 凭证含 certificateKey | `internal/join/token`（token create --role master） | 每次 master 签发轮换；已有 master 命令立即失效 |
| 证书包 Secret 无 TTL | `internal/join/certs/secret.go` | 密文长期存在，key 长期有效 |
| master 组可读证书包 | `internal/clusterinit/clusterinit.go`（`joinCertsRole`） | 任何 master 令牌都能读 |
| CSR 自动审批 | `internal/clusterinit/clusterinit.go`（`nodeclient` 绑定） | 无 CN/节点绑定 |
| 客户端 role 校验 | `internal/install/join.go`（`validateJoinRole`） | 只防手滑，不防攻击者 |
| token 过期由服务端控制 | `kube-system/bootstrap-token-*` 的 expiration | 客户端不做预检查，过期后才在 apiserver 处失败 |
| 无 token 吊销命令 | `internal/cli` | 只能手动 `kubectl delete secret` |
| join 从 cluster-info 取 clusterID 并写入 node.yaml | `internal/install/join.go`（`adoptClusterID`） | cluster-info 的 JWS 已校验；旧集群没有 clusterID 时为空 |
| 证书包 CA 与 join 的 CA pin 二次比对 | `internal/install/join.go`（`VerifyCACertHashes`） | 覆盖 cluster-info 与证书包两条通道不一致 |
| 证书包写入前全量冲突预检 | `internal/join/certs/bundle.go`（`WriteTo`） | 需要 `--replace-pki` 才能覆盖 CA/SA/加密配置 |
| worker TLS Bootstrap 后删除引导 kubeconfig | `internal/phase/kubelet`（`cleanupBootstrapKubeconfig`） | 降低 token 在磁盘上的残留窗口 |
| join 支持从文件读 token/certificateKey | `kubemono join --token-file` / `--certificate-key-file` | 避免 argv/`/proc` 暴露 |

master 凭证对应的证书包包含 `pki/ca.key`、`pki/sa.key`、`pki/etcd/ca.key`、`pki/front-proxy-ca.key`
和 `encryption/config.yaml`（`internal/join/certs/bundle.go` 的 `Build` 把加密配置列为 required），
所以它确实是“集群接管套件”。

### 13.2 当前安全基线

1. **默认不落盘**：`init` 不生成 join 物料；`token create` 只打印一条命令，供操作者立即使用。
2. **统一 2h 硬上限**；如需更短，再拆分 master/worker 的 TTL。
3. **按角色签发**：默认 worker 不碰 certificateKey；master 必须显式 `--role master`。
4. **`token create` 输出风险说明**，提示 token 会进入 shell history/CI 日志。
5. **尚未提供 `kubemono token revoke <token-id>`**（封装删除 Secret、清理 cluster-info JWS + 审计）。
6. **尚未提供 join 成功后的“令牌将在 X 分钟后过期、无需保留”提示**。
7. **文档与 CLI 都强调凭证不落盘**，让操作者知道保留输出没有长期价值。

当前 2h TTL 仍允许窗口内复用/跨节点；要达到第 4 节的 I1（分钟级）和 I2（一次性），
需要后续的票据/审批模型。

### 13.3 后续演进：join broker

在 `kube-system` 部署一个小控制器（Deployment 或 static pod），职责：

- **票据注册**：用标准 bootstrap token Secret + 注解承载 `node`、`role`、`ticket-id`、`used`；
  KCM 的 tokencleaner 负责过期清理。
- **CSR approver**：按 8.2 的清单校验；通过后标记 used、删除 token、发 Event。
- **GC**：删除过期的响应 Secret、证书包 Secret；删除已消费的票据。
- **审计**：结构化日志 + Kubernetes Event + 指标。
- **RBAC**：get/list/watch secrets（kube-system）、update/patch CSRs、get nodes、create events、
  delete 指定名字的响应 Secret（`resourceNames`）；不给 `secrets/*` 全局写。
- **enroll/install 解耦**：当前 `join` 直到 kubelet 阶段才用令牌发起 CSR，慢安装可能超过 10 分钟；
  演进方向要么把“取证书”前移成 `enroll` 子命令，要么给批量场景 30–60 分钟的票（见 11.6）。

CLI：

```bash
kubemono join ticket create --node node-7 --role worker --ttl 10m
kubemono join ticket list
kubemono join ticket revoke jt-7f3a9c
kubemono join -f cluster.yaml --ticket-stdin   # 或交互式提示
```

### 13.4 后续演进：信封加密（2–4 周）

- joiner 生成 X25519 临时密钥；
- 通过 CSR 注解（或专用 `JoinRequest` Secret）把 `ephJ_pub`、node、ticket 交给 broker；
- broker 校验后，把证书包加密到 `ephJ_pub`，写入一个**只对该 token 用户可读**的响应 Secret
  （每票一个 RoleBinding，`resourceNames` 精确到该 Secret）；
- joiner 读取、解密、写 PKI，然后删除响应 Secret；
- broker 在交付后删除响应和证书包；证书包只由临时密钥解密，不使用 `certificateKey`。

收益：即使证书包 Secret 被读、被备份、被日志记录，没有临时私钥也解不开；共享静态 key 消失。

### 13.5 后续演进：身份化与 CA 托管（按需）

1. **云实例身份**：AWS IMDSv2 实例身份文档 / GCP identity token / Azure MSI；broker 验签 + 清单匹配。
2. **TPM 证明**：EK 证书 + AK quote + nonce；清单里存 EK 指纹；适合自建机房。
3. **中间 CA**：根 CA 离线；每个 master 一张中间 CA；KCM 用中间 CA 签名；泄露只影响一个 master。
4. **HSM/KMS 签名**：根 CA 不出硬件；需要自定义 signer，工作量最大。

### 13.6 演进验收

| 阶段 | 验收标准 |
|---|---|
| 当前 | 过期凭证由服务端拒绝；master/worker 物料分离；`token create` 不落盘 |
| broker | 同一票据第二次使用被拒；worker 票不能为其他节点名申请证书；master 票需要审批；成功后 token 被删 |
| 信封加密 | 证书包只对目标机器临时公钥可解；响应 Secret 交付后删除；`certificateKey` 不出现在凭证中 |
| 身份化 | 云上无票据加入；TPM 节点无票据加入；单 master 失陷不泄露根 CA |

---

## 14. 红队验收用例

| # | 场景 | 期望结果 | 验证点 |
|---|---|---|---|
| A | 拿到 join 文件，15 分钟后再用 | 失败（过期） | 服务端 TTL 为准 |
| B | 用合法节点加入后，再用同一文件加入第二台 | 失败（已消费） | 票据状态 |
| C | 把 node-7 的票拿到 node-9 上用 | 失败（节点不匹配） | CN 绑定 |
| D | 用 worker 票读 `kubemono-certs` | 失败（RBAC 拒绝） | 角色隔离 |
| E | 用 worker 票申请 `CN=system:node:<已有节点>` | 失败（approver 拒绝） | 已有 Node 检查 |
| F | 把文件留在磁盘 30 天后再用 | 失败（无有效凭证） | 无长期密钥 |
| G | 假 apiserver / 错误 CA 中间人 | 失败（pin 校验） | CA pin |
| H | master 加入但审批人未批准 | 失败（pending） | 审批状态机 |
| I | 成功加入后 10 秒内检查 token Secret | 已删除 | 消费逻辑 |
| J | 审计查询“谁在何时从哪加入” | 可回答 | 审计字段 |
| K | 从 30 天前的备份恢复 join 文件 | 失败 | TTL 穿透备份 |
| L | 使用蜜罐票据 | 告警 | 检测能力 |

最关键的三个：**A（过期）、B（一次性）、C（绑定）**。它们直接对应“忘记删文件”的风险。

---

## 15. 反模式清单

1. **长期共享 token + 文件分发**（L0）：方便，但等于永久后门。
2. **把“用后删除”当唯一控制**：不可验证、不可审计。
3. **24h TTL 就宣称“短期”**：覆盖整个 rollout。
4. **一个令牌给所有节点、所有角色**：无法归因、无法单独吊销。
5. **token 和 certificateKey 放同一个文件**：两因素变一因素。
6. **证书包 Secret 永久保留**：key 的有效期等于密文的生命周期。
7. **CSR 全自动审批、无 CN 绑定**：任何令牌可为任意节点名申请证书。
8. **只在客户端校验角色**：攻击者自己写 YAML。
9. **`--insecure-skip-tls-verify` / http:// 端点**：令牌裸奔。
10. **token 走 argv、URL、CI 日志**：`ps`、日志、回滚缓冲区都会留。
11. **把 join 文件当 DR 备份**：它既可能过期，也可能被轮换；DR 靠 etcd 快照 + 根 CA 保险库。
12. **把 0600 当安全边界**：root、备份、快照、快照恢复都不看权限位。
13. **用“私有网络/私有仓库”代替凭证管理**：横向移动和仓库泄露都很常见。
14. **master join 不做审批**：等于把根 CA 的复制权交给任何拿到文件的人。

---

## 16. 附录

### A. 与主流实现对比

| 实现 | 凭证 | 寿命/次数 | 绑定 | master 物料 | 主要风险 |
|---|---|---|---|---|---|
| k3s | 集群 token（server/agent 共用） | 长期，可轮换 | 角色（token 前缀） | 控制面节点拿全部 secrets | 长期共享密钥；token 常被写进脚本 |
| k0s | kubeconfig 形式的 join token | 可配 `--expiry` | 角色 | 控制面 join 拿 CA | 默认倾向长期；文件分发 |
| RKE2 | server token | 长期，可轮换 | 角色 | 控制面 join 拿 secrets | 长期共享密钥 |
| Talos | machine config + join token | 配置即秘密 | 角色/集群 | machine config 含 secrets 包 | 配置文件是“皇冠上的珠宝” |
| EKS | 节点 IAM role + aws-auth | 无共享密钥 | 云身份 | 控制面由云托管 | 依赖云 IAM 正确性 |
| GKE | 节点 service account | 无共享密钥 | 云身份 | 控制面由云托管 | 依赖云 IAM 正确性 |
| Cluster API | bootstrap token / 云身份 | 短 | 机器/云 | 由 provider 管理 | 取决于 provider |
| SPIRE | join token / 云/TPM 证明 | 短 | 工作负载/节点 | 不适用 | 需要额外基础设施 |

结论：**行业方向是“短命 + 一次性 + 证明”，文件式 join 仍然存在，主要是为了自建/离线/低集成成本的场景。**
kubemono 的定位（自建、离线、多 master）决定了它不能直接照搬云厂商的“无票据”，但可以做到 L3，
并把 L4 作为可选。

### B. 审计事件

| 事件 | 关键字段 |
|---|---|
| `join.ticket.create` | ts, clusterID, ticketID, actor, node, role, ttl, sourceIP |
| `join.ticket.revoke` | ts, ticketID, actor, reason |
| `join.claim` | ts, ticketID, node, sourceIP, userAgent, result |
| `join.approve` | ts, ticketID, actor, method（auto/manual/two-person） |
| `join.deny` | ts, ticketID, node, reason, csrName |
| `join.bundle.deliver` | ts, ticketID, node, cipher（x25519/none）, result |
| `join.consume` | ts, ticketID, node, csrName, certFingerprint |
| `join.reuse_attempt` | ts, ticketID, node, sourceIP（**告警**） |
| `join.node.register` | ts, node, role, sourceIP |
| `join.master.verify` | ts, node, apiserverCertFingerprint, etcdMemberID |

审计要写到集群外（SIEM/对象存储），避免“攻击者删了集群内日志”。

### C. 术语

- **Bootstrap token**：Kubernetes 的引导令牌，`<id>.<secret>`，用于 TLS Bootstrap。
- **TLS Bootstrap**：kubelet 用引导凭证申请客户端证书的过程。
- **CSR approver**：决定是否批准证书签名请求的组件/控制器。
- **Capability**：持有即有权的能力凭证。
- **Envelope encryption**：用一次性密钥加密数据、再用长期密钥加密该密钥；本文特指用临时公钥直接加密响应。
- **Intermediate CA**：由根 CA 签发的下级 CA，可单独吊销/轮换。
- **JIT（just-in-time）**：按需、短时地授予权限。
- **TOFU**：首次使用即信任（trust on first use）。

### D. 参考

- Kubernetes: Bootstrap Tokens — https://kubernetes.io/docs/reference/access-authn-authz/bootstrap-tokens/
- Kubernetes: TLS bootstrapping — https://kubernetes.io/docs/reference/access-authn-authz/kubelet-tls-bootstrapping/
- Kubernetes: Certificate Signing Requests — https://kubernetes.io/docs/reference/access-authn-authz/certificate-signing-requests/
- Kubernetes: Node authorization — https://kubernetes.io/docs/reference/access-authn-authz/node/
- Kubernetes: Encrypting data at rest — https://kubernetes.io/docs/tasks/administer-cluster/encrypt-data/
- SPIFFE/SPIRE concepts — https://spiffe.io/docs/latest/spire-about/spire-concepts/

---

## 附：一句话回答“那到底要不要删文件？”

**要删，但删不是安全措施。** 删是卫生习惯；真正的安全措施是：这张票 10 分钟后就死、只能用一次、
只能在这一台机器上用。做到这三点，文件留在磁盘上也只是过期垃圾，不是后门。
