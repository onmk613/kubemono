# BOM 与物料版本参考

BOM（Bill of Materials，`internal/bom/bom.yaml`）是 kubemono 的**版本与组件来源唯一事实来源**：
它决定这个 kubemono 二进制默认使用哪些组件、版本是多少、从哪里下载、镜像引用是什么。
`cluster.yaml` 只在此基础上做**少量覆盖**（Kubernetes 版本、非 k8s 组件版本、镜像引用），
`bundle pack` 再把合并后的结果变成可导入的物料归档。

本文回答三件事：

1. `bom.yaml` 有哪些字段、哪些能改、校验规则是什么；
2. 改版本、改下载地址、改镜像时，应该用 `cluster.yaml` 还是 `--bom` / 重新编译；
3. 这些改动如何与 `bundle pack`、`bundle load`、`init/join/node repair` 配合。

配套文档：字段级配置见 [`cluster-config.md`](cluster-config.md)，命令与参数见
[`cli-reference.md`](cli-reference.md)。

- 文件位置：`internal/bom/bom.yaml`
- 加载方式：`//go:embed` 编译进二进制；节点侧**不读取**磁盘上的独立 BOM 文件
- 严格解析：未知字段直接报错
- 校验入口：`bundle pack --dry-run`、`bundle list`，以及所有加载 `cluster.yaml` 的命令
  （它们会先合并覆盖再校验）

---

## 1. BOM 在哪里被读取

| 场景 | 使用的 BOM | 说明 |
|---|---|---|
| `bundle pack` / `bundle list`（计划模式） | 内嵌 BOM，或 `--bom` 指定的完整 BOM | 再叠加 `-f cluster.yaml` 的版本/镜像覆盖 |
| `bundle verify` | **不读 BOM** | 只按归档 `meta.json` 校验 |
| `bundle load` | **不按 BOM 导入** | 只按 `meta.json` 导入；存在 `cluster.yaml` 时读取生效 release 做一致性提示 |
| `init` / `join` / `node repair` | **内嵌 BOM + `cluster.yaml` 覆盖** | 节点上无法指定 `--bom` |
| `cert renew` / `config validate` | 内嵌 BOM + `cluster.yaml` 覆盖 | `config validate` 会检查版本/镜像覆盖是否指向已知组件 |
| `pki issue` / `cert show` | **不读 BOM** | 纯离线证书工具 |
| `kubemono version` | **只读内嵌 BOM** | 不读取 `cluster.yaml` 覆盖 |

由此得到最重要的一条边界：

> **`--bom` 只影响打包侧（`bundle pack`/`bundle list`），不会分发到节点。**
> 节点侧的版本、镜像与 release 由「节点二进制内嵌的 BOM + 该节点上的 cluster.yaml」决定。
> 任何会改变节点行为的 BOM 修改，必须在节点侧有对应的 `cluster.yaml` 覆盖，或者重新编译
> kubemono 让新 BOM 成为内嵌默认值。

---

## 2. 文件结构

```yaml
release: v1.37.1-km.1        # kubemono 发行号
kubernetes: v1.37.1          # 本 BOM 的 Kubernetes 基线
arch: [amd64, arm64]         # 支持的架构（只允许这两个）

components:
  <组件名>:
    version: v1.2.3          # 必填
    category: runtime        # 必填，见 §3.5
    source: https://...      # 可选：下载 URL 模板
    binaries: [name1, name2] # 可选：该组件提供的可执行文件
    image: registry/...:tag  # 可选：完整镜像引用模板
    notes: "..."             # 可选：仅供阅读，不参与任何逻辑
```

### 2.1 顶层字段

| 字段 | 必填 | 说明 |
|---|---|---|
| `release` | 是 | kubemono 发行号。格式 `v<major>.<minor>.<patch>[-<prerelease>]`，可带 `-km.<n>` 后缀；`km.N` 的 N 必须是正整数 |
| `kubernetes` | 是 | Kubernetes 基线版本，例如 `v1.37.1`；BOM 里所有 `category: k8s` 组件的 `version` 必须与它一致 |
| `arch` | 是 | 支持的架构列表，非空、无重复，只允许 `amd64`/`arm64`；`--arch` 取值受它约束 |
| `components` | 是 | 组件表，不能为空；组件名不能是 `kubemono`（kubemono 自身是独立二进制，不在物料里） |

`release` 的写法示例与含义：

| 写法 | 含义 | 是否合法 |
|---|---|---|
| `v1.37.1-km.1` | Kubernetes v1.37.1 + kubemono patch 1 | 合法（推荐） |
| `v1.37.1` | 纯上游版本，没有 kubemono patch | 合法 |
| `v1.37.1-km` | 缺少 `.n` | 拒绝 |
| `v1.37.1-km.0` | patch 必须 ≥ 1 | 拒绝 |
| `1.37.1-km.1` | 必须以 `v` 开头 | 拒绝 |
| `v1.37` | 必须三段 | 拒绝 |

`release` 不只是展示字符串：它是物料目录、`meta.json`、节点身份 `node.yaml` 与
`kube-public/cluster-info` 的键。**同一 release 不应对应两份内容不同的物料**。

---

## 3. 组件字段

### 3.1 字段表

| 字段 | 必填 | 说明 |
|---|---|---|
| `version` | 是 | 组件版本。宽松格式：`v1.14.6`、`2.8`、`3.10.2` 都可以；用于占位符展开、物料 meta 与展示，不做版本范围校验 |
| `category` | 是 | 组件类别，决定物料内目录 `bin/<category>/<name>` 与列表顺序 |
| `source` | 否 | 下载 URL 模板；为空表示该组件不从网络获取（通常配合 `image`） |
| `binaries` | 否 | 该组件提供的可执行文件名；打包时从 `source` 中提取这些文件 |
| `image` | 否 | 完整镜像引用模板（含 registry host 与 tag/digest） |
| `notes` | 否 | 注释性文字，当前不参与任何逻辑 |

一个组件至少要能提供点什么：`source` + `binaries`（二进制）或 `image`（镜像）。
两者都为空的组件可以写，但打包时会被跳过、节点也不会使用。

### 3.2 占位符

`source` 与 `image` 都支持模板替换：

| 占位符 | 替换为 | 可用于 |
|---|---|---|
| `{version}` | 组件 `version` 原样，如 `v2.4.2` | source、image |
| `{versiontrim}` | 去掉前导 `v` 的版本，如 `2.4.2` | source、image |
| `{kubernetes}` | BOM 的 `kubernetes` | source、image |
| `{release}` | BOM 的 `release` | source、image |
| `{arch}` | 打包时的目标架构，如 `amd64` | **仅 source**（image 不使用架构） |

示例：

```yaml
containerd:                       # 二进制组件：source + binaries
  version: v2.4.2
  source: https://github.com/containerd/containerd/releases/download/{version}/containerd-{versiontrim}-linux-{arch}.tar.gz

coredns:                          # 镜像组件：image
  version: v1.14.6
  image: registry.k8s.io/coredns/coredns:{version}
```

### 3.3 `source` 与 `binaries`

`source` 是 `bundle pack` 的下载地址，必须能通过 HTTP(S) 获取：

- 支持两种来源形态：
  - **归档**：`.tar.gz`、`.tgz`、`.tar`；打包时只提取 `binaries` 列出的成员，
    **按 basename 匹配**（归档内部的目录层级会被消化掉）；
  - **单文件**：此时 `binaries` 必须**恰好一个**，否则打包报错；
- 归档里缺少 `binaries` 中任何一个文件时，`bundle pack` 直接失败，并打印归档的前几个条目；
- 下载缓存路径为
  `<cache-dir>/<release>/<component>/<arch>/<URL + 组件版本的 sha256 前 12 位>/<URL basename>`；
  **改 URL 或组件版本都会让缓存失效**（即使 mirror 的稳定 URL 不含 `{version}`）；
- `{arch}` 只能出现在 `source` 里；多架构要分别打包（`--arch` 一次一个）。

单文件来源示例：

```yaml
runc:
  version: v1.5.2
  category: runtime
  binaries: [runc]
  source: https://github.com/opencontainers/runc/releases/download/{version}/runc.{arch}
```

### 3.4 `image`

- 必须写**完整引用**：包含 registry host 与 tag 或 digest，例如
  `registry.k8s.io/kube-proxy:v1.37.1`；校验会拒绝 `kube-proxy:v1` 这种依赖隐式 Docker Hub 的写法；
- 支持 `{version}`、`{versiontrim}`、`{kubernetes}`、`{release}`，但**不支持 `{arch}`**；
- 只有声明了 `image` 的组件才能被 `cluster.yaml` 的 `images.components` 覆盖；
- `bundle pack --images` 只把 BOM 声明的镜像放进归档，并且按目标平台（`linux/<arch>`）裁剪；
  缓存 layout 里的其它镜像不会被打包。

### 3.5 `category`

| 类别 | 典型组件 | 物料路径 |
|---|---|---|
| `k8s` | kube-apiserver、kube-controller-manager、kube-scheduler、kubelet、kube-proxy、kubectl | `bin/k8s/<name>`（有 `binaries` 时） |
| `etcd` | etcd（含 etcdctl/etcdutl） | `bin/etcd/<name>` |
| `runtime` | containerd、runc | `bin/runtime/<name>` |
| `cni` | cni-plugins 的 18 个插件 | `bin/cni/<name>` |
| `network` | kube-router | 通常只有镜像 |
| `addon` | coredns | 通常只有镜像 |
| `lb` | haproxy | 通常只有镜像 |
| `image` | pause | 只有镜像 |

类别同时决定：

- 物料内路径 `bin/<category>/<name>`（**节点按自己的 BOM 类别查找二进制**）；
- `bundle list` / 归档内组件的排序；
- `bundle list --category` 的过滤值。

**不要随意改既有组件的 `category`**：节点会按内嵌 BOM 的类别去 `bin/<category>/<name>`
找文件，改了类别会让安装阶段找不到二进制（除非同时重建 kubemono 二进制）。

---

## 4. 校验规则

`bom.Parse` 使用严格 YAML 并执行以下校验，任何一条不满足都会让 `bundle pack`/`bundle list`
直接失败：

1. `release` 非空且能被 `ParseRelease` 解析（见 §2.1）；
2. `kubernetes` 非空；
3. `arch` 非空、无重复，只能是 `amd64`/`arm64`；
4. `components` 非空，且不能包含名为 `kubemono` 的组件；
5. 每个组件 `version`、`category` 非空，`category` 必须是 §3.5 的枚举；
6. 每个 `image` 展开后必须是合法完整引用（registry host + tag/digest）；
7. 全部 `category: k8s` 组件的 `version` 必须等于顶层 `kubernetes`。

`source` 的 URL 格式**不在** BOM 校验阶段检查；下载时才会暴露错误（见 §10）。

---

## 5. 生效 BOM 是怎么合并出来的

```text
内嵌 BOM（或 --bom 指定的 BOM）
  └─ ① 版本覆盖
       ├─ kubernetesVersion → 全部 k8s 组件整组同版本
       └─ images.componentVersions → 非 k8s 组件逐个覆盖
  └─ ② k8s 组版本语义：release 改写为 v<kubernetesVersion>-km.<原 km 号>
  └─ ③ images.components → 按组件替换完整镜像引用
  └─ 生效 BOM
       ├─ bundle pack：按它下载/裁剪物料
       └─ 节点：按它安装组件、渲染镜像、记录 release
```

规则细节：

- **k8s 组件必须整组同版本**：`kubernetesVersion` 会同时改 kube-apiserver、
  kube-controller-manager、kube-scheduler、kubelet、kube-proxy、kubectl；
  不允许在 `componentVersions` 里单独写 k8s 组件；
- k8s 版本与 BOM 基线不同时，`release` 会从 `v1.37.1-km.1` 改写为
  `v1.37.4-km.1`（保留 `km.N`）；与基线相同时 release 不变；
- 非 k8s 组件版本覆盖不改变 `release`，只打印 warning；
- 镜像覆盖最后应用，只允许覆盖原本有 `image` 的组件；
- 覆盖里出现未知组件、k8s 组件（版本覆盖）、`kubemono` 自身，都会报错；
- `bundle pack` 与节点命令使用**同一套合并函数**，所以只要两侧输入一致，
  release、版本与镜像就一致。

---

## 6. 三种设置渠道：该用哪个

| 渠道 | 能改什么 | 谁生效 | 需要重新编译 kubemono | 需要重建物料 |
|---|---|---|---|---|
| `cluster.yaml` `kubernetesVersion` | k8s 整组版本（并改 release） | `pack` + 节点 | 否 | 是（对应 release） |
| `cluster.yaml` `images.componentVersions` | 非 k8s 组件版本 | `pack` + 节点 | 否 | 是 |
| `cluster.yaml` `images.components` | 镜像完整引用 | `pack` + 节点 | 否 | 是（镜像要进包或推到仓库） |
| `bundle pack/list --bom` | 整份 BOM（含 source、版本、镜像、arch、release） | **只有 pack/list** | 否 | 是 |
| 修改 `internal/bom/bom.yaml` 后重新编译 | 全部（成为新默认值） | 所有命令与所有节点 | **是** | 是 |

结论：

- **版本与镜像**：优先用 `cluster.yaml`。两侧（打包与节点）自动一致，不需要自定义 BOM；
- **下载地址（source，例如内网镜像站）与 pack 侧组件清单**：用 `--bom`，这些只影响打包；
- **只改 `arch` 列表**：`--bom` 即可影响 `bundle pack --arch` 的合法取值；节点二进制本身
  仍需能在目标架构运行；
- **改默认值 / 改 category / 改 binaries / 增删组件 / 长期维护自定义发行版**：
  修改 `internal/bom/bom.yaml` 并重新编译。

---

## 7. 用 `cluster.yaml` 改版本

### 7.1 改 Kubernetes 版本

```yaml
kubernetesVersion: v1.37.4
```

- `release` 自动变成 `v1.37.4-km.1`（沿用 BOM 的 `km.1`）；
- 必须准备对应 release 的物料：
  `kubemono-v1.37.4-km.1-linux-amd64.tar.zst`；
- 只能在**新建集群**时设置；已有集群换版本 = 新建集群 + 迁移工作负载；
- `kubemono version` 仍然显示内嵌 BOM 的版本（不会读这个覆盖）。

打包与安装：

```bash
# 构建机：按覆盖后的 BOM 出包（release 已改写）
kubemono bundle pack -f cluster.yaml --images

# 节点：导入同一 release 的物料
kubemono bundle load kubemono-v1.37.4-km.1-linux-amd64.tar.zst
kubemono init -f cluster.yaml
```

### 7.2 改非 k8s 组件版本

```yaml
images:
  componentVersions:
    coredns: v1.14.7
    containerd: v2.4.3
```

- 只允许 BOM 中存在的非 k8s 组件；`kubemono` 自身与 k8s 组件会被拒绝；
- 与默认版本不同时只 warning；非默认版本没有预置物料，必须自己准备；
- 节点级组件（`etcd`、`runtime`、`cni`）改版本时，建议按新物料重建节点，
  而不是只做在线 `node repair`；
- `release` **不变**，同一 release 下会出现不同版本的物料，注意区分。

### 7.3 改镜像引用

```yaml
images:
  components:
    pause:      registry.example.com/kubemono/pause:3.10.2
    coredns:    registry.example.com/kubemono/coredns/coredns:v1.14.6
    kube-proxy: registry.example.com/kubemono/kube-proxy:v1.37.1
```

- 必须写全（registry host + tag/digest）；
- key 必须是 BOM 中声明了 `image` 的组件（当前：`pause`、`kube-proxy`、
  `kube-router`、`coredns`、`haproxy`）；
- 只改“引用”，不提供“拉取路径”；私有仓库认证仍然配在
  `node.runtime.containerd.registries`，mirror 也是另一套机制；
- 改完必须重新出包或把镜像推到节点可达的仓库，否则节点拉不到镜像。

---

## 8. 用 `--bom` 改下载地址（打包侧）

`--bom` 接受一份**完整**的 BOM 文件（不是补丁/片段），只被 `bundle pack` 与
`bundle list` 的计划模式接受。

最典型的用法：内网镜像站替换上游下载地址。

```bash
# 1) 以仓库里的 BOM 为底
cp internal/bom/bom.yaml ./bom-internal.yaml

# 2) 只改 source（保留 version/category/binaries/image）
#    containerd:
#      source: https://mirror.example.com/containerd/containerd-{versiontrim}-linux-{arch}.tar.gz
#    kube-apiserver:
#      source: https://mirror.example.com/k8s/{version}/bin/linux/{arch}/kube-apiserver

# 3) 先看计划（不联网、不写盘）
kubemono bundle list  --bom ./bom-internal.yaml -f cluster.yaml
kubemono bundle pack  --bom ./bom-internal.yaml -f cluster.yaml --dry-run

# 4) 出包
kubemono bundle pack --bom ./bom-internal.yaml -f cluster.yaml --images
```

### 8.1 哪些改动是安全的

| 改动 | 对节点的影响 | 结论 |
|---|---|---|
| `source` URL | 节点从不读取 source；只要**内容与版本仍是同一份**，节点无感知 | 安全，`--bom` 的标准用法 |
| `notes` | 不参与逻辑 | 安全 |
| `arch` 列表 | 只影响 `--arch` 校验；节点不读 | 安全（但节点二进制若只支持某架构，需自行保证） |
| 新增节点不使用的组件 | 节点只安装自己 BOM 声明的组件；多余文件留在物料里 | 一般安全，但会增加归档体积 |
| 减少组件 / 改 `binaries` | 节点按自己的 BOM 查找文件；缺文件会让安装失败 | **不安全**，除非同时重建二进制 |
| 改 `category` | 节点从 `bin/<category>/<name>` 查找；路径对不上会失败 | **不安全**，除非同时重建二进制 |
| 改 `version` / `image` | 节点会用**自己的** BOM 版本与镜像引用渲染；与包内不一致会导致镜像 tag 不匹配或记录错版本 | **不保证**；请改用 `cluster.yaml` 覆盖，或重建二进制 |
| 改 `release` | 节点按自己的 release 找 `/usr/local/kubemono/artifacts/<release>` | **不安全**；物料会装到节点不会读取的目录 |

如果确实要用 `--bom` 改版本/镜像，同时让节点也生效，必须在节点侧的 `cluster.yaml` 写
相同的 `images.componentVersions` / `images.components`（见 §7），让两侧算出同一份生效 BOM。
这属于“包装自定义发行版”的用法，推荐直接走 §9。

### 8.2 自定义 BOM 的校验

- 必须是完整 BOM：`release`、`kubernetes`、`arch`、`components` 缺一不可；
- 未知字段直接报错（包括拼错的 `binaries`、`category`）；
- 与内嵌 BOM 一样，k8s 组件版本必须与 `kubernetes` 一致；
- 合并 `-f cluster.yaml` 后仍会执行版本/镜像覆盖校验；覆盖里引用自定义 BOM 中不存在的
  组件会报错。

---

## 9. 自定义发行版：改内嵌 BOM

当你要长期改变默认值（公司内部镜像站、固定的组件版本、增删组件、改 category/binaries、
新增架构），正确做法是修改 `internal/bom/bom.yaml` 并重新构建：

```bash
# 1) 编辑 BOM
vi internal/bom/bom.yaml
#    - 改默认版本 / source / image
#    - 变更内容时建议 bump release 的 km.N（避免同一 release 对应两份物料）

# 2) 构建与出包
make build-linux
make dist ARCHS=amd64 IMAGES=1

# 3) 验证
kubemono version
kubemono bundle list
kubemono bundle verify dist/kubemono-<release>-linux-amd64.tar.zst
```

注意：

- 节点二进制与物料必须来自**同一次构建**；旧二进制配新 BOM 物料会出现 release 或
  类别路径不一致；
- `release` 是节点记录与物料目录的键，变更内容后 bump `km.N` 可以避免
  “同一 release 两份内容”的审计问题（BOM 校验不会替你检查这一点）；
- 改 `release` 后，已有集群无法原地使用新物料（节点记录的是旧 release）；
  换发行版仍按“新建集群 + 迁移”处理。

---

## 10. `bundle pack` 如何使用 BOM

### 10.1 组件

1. 按 `BOM.Names()`（类别顺序 + 名称）遍历组件；
2. `source` 为空或 `binaries` 为空的组件跳过；
3. 计算 `<cache-root>/<release>/<component>/<arch>/<url+version-hash>/<basename>` 缓存路径；
   缓存缺失时下载（`--offline` 下直接报错，`--refresh` 强制重下）；
4. 归档来源按 basename 提取 `binaries`；单文件来源要求恰好一个二进制；
5. 写入物料暂存目录 `bin/<category>/<name>`，权限 0755。

### 10.2 镜像

- 只有 `--images` 才收集镜像；
- 需要的引用来自 `BOM.Images()`（展开占位符、去重）；
- 源是 OCI layout（默认 `<cache-root>/<release>/images`；`pack.Options.ImagesDir` 可覆盖，
  但 CLI 未暴露该参数）；只复制 BOM 需要的引用与目标平台；
- `--fetch-images=false` 或 `--offline` 时不会拉取缺失镜像，直接报错；
- 可复现构建：`--created 2026-01-01T00:00:00Z` 或 `SOURCE_DATE_EPOCH=<unix 秒>` 固定 meta
  与 tar mtime（zstd 压缩级别固定），同一输入可得到相同归档摘要。

### 10.3 产物

```text
kubemono-<release>-linux-<arch>.tar.zst        # --images
kubemono-<release>-linux-<arch>-lite.tar.zst   # 默认（轻量包）
kubemono-<release>-linux-<arch>[-lite].tar.zst.sha256
```

归档内：

```text
meta.json          # release/kubernetes/arch/os/created/components/images/files
bin/<category>/<name>
images/            # OCI layout（仅完整包）
```

`meta.json` 是 `bundle load` 与 `bundle verify` 的权威清单；节点安装用
`meta.json` 定位文件并校验 sha256，但**版本与镜像引用仍由节点自己的
`cluster.yaml` + 内嵌 BOM 重新解析**。因此 pack 侧改版本/镜像后，节点配置必须
同步修改并重新 `bundle load`，否则会出现“物料是新的、渲染引用是旧的”（或反之）。
`bundle load` 会对比 release；`init`/`join`/`node repair` 在开始执行前还会把
`meta.json` 的 Kubernetes 版本、组件版本与镜像清单和生效 BOM 做比较，不一致
直接失败（同 release 重新出包也不会静默混用）。`--bom` 只影响打包侧：要让节点
接受自定义 BOM 的物料，节点也必须使用同一份 BOM 构建。

---

## 11. 校验与排错

推荐顺序：

```bash
kubemono bundle list                                              # 计划：组件 + 版本 + 本地物料状态
kubemono bundle list --images                                     # 计划：镜像引用
kubemono bundle pack --dry-run                                    # 计划：缓存/镜像/输出路径
kubemono bundle pack --images                                     # 出包
kubemono bundle verify dist/kubemono-<release>-linux-amd64.tar.zst
kubemono bundle load   dist/kubemono-<release>-linux-amd64.tar.zst
```

常见错误：

| 报错 | 原因与处理 |
|---|---|
| `parse BOM: ... unknown field "xxx"` | 自定义 BOM 里有未知/拼错字段；严格解析不接受多余字段 |
| `bom: release is required` / `invalid kubemono release ...` | `release` 为空或格式不对（见 §2.1） |
| `bom: unsupported arch "..." (only amd64 and arm64)` | `arch` 只允许 `amd64`/`arm64` |
| `bom: component "kubemono" must not be in the BOM` | 删掉 `components.kubemono` |
| `bom: component "kube-apiserver" version ... does not match kubernetes ...` | k8s 组件版本必须整组等于顶层 `kubernetes` |
| `bom: component "xxx" image "...": must be a full image reference including registry host` | 镜像要写全 registry host；tag/digest 也要有 |
| `component "xxx" is not in BOM release ...` | `cluster.yaml` 的覆盖引用了 BOM 中不存在的组件（或名字拼错） |
| `component "xxx" has no image to override` | 该组件没有 `image`，不能写进 `images.components` |
| `k8s 组件必须整组固定（...）：缺少 ...` | 自定义 BOM 缺少某个 k8s 组件，或版本覆盖只改了一部分 |
| `归档 ... 中没有 xxx（前几个条目：...）` | `binaries` 与上游归档内容不一致；按报错里的条目名修正 |
| `组件 xxx: 有 N 个二进制但来源 ... 不是归档` | 单文件来源只能声明一个二进制 |
| `架构 "..." 不在 BOM 的 arch 列表里` | `--arch` 不在 `arch` 列表；一次只打一个架构 |
| `artifact: OCI layout 不存在：...` | `--images` 需要 OCI layout；先让 pack 拉取（默认）或把 layout 放到正确目录 |
| `物料 release=...，cluster.yaml 生效 release=...` | `bundle load` 的提示：物料 release 与当前配置不一致；确认不是误装 |
| `物料库里有多个 release（...），请用 --release 或 -f 指定` | `bundle verify` 库模式的 release 解析需要显式指定 |

---

## 12. 与其它文档的分工

| 想知道 | 看哪里 |
|---|---|
| `cluster.yaml` 字段、默认值、约束 | [`cluster-config.md`](cluster-config.md) |
| 命令与参数（`bundle pack/load/verify`、`node repair` 等） | [`cli-reference.md`](cli-reference.md) |
| BOM 字段、版本/URL/镜像规则、打包机制 | 本文 |
| 改配置后跑哪条命令、能否原地 | [`cli-reference.md` §14](cli-reference.md) + `cluster-config.md` §8 |
