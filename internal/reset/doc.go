// Package reset 实现本机重置。
// 严格按 的五步执行：
// 1. 停止并禁用控制面 unit；
// 2. 停止 kubelet、用 ctr 停止容器、卸载 kubelet 数据目录下的挂载点；
// 3. 原生网络清理（iptables/ipvs/netlink/ipset，不依赖任何组件二进制）；
// 4. 备份 PKI 后删除 /etc/kubernetes 与 kubelet/etcd 数据目录（--keep-data 时保留）、
// unit 文件和 /usr/local/kubemono；/usr/local/bin 与 /opt/cni/bin 下的二进制保留（只停服务，
// 不删除可能被运行时或其他组件引用的可执行文件）；
// --purge 额外删除 BOM 管理的二进制、CNI 插件、containerd 配置/数据、运行时 unit
// 与 /usr/local/kubemono；PKI 备份仍在 /usr/local/kubemono/backup/<timestamp>/，
// 成功 purge 后随根目录一起删除（失败时保留用于恢复）；
// 5. 不带 --keep-hostinit 时还原主机初始化；不带 --keep-data 时删除 etcd/kubelet 数据目录。
// 不可逆性由三件事保证：
// - DryRun 不执行任何会改主机的动作，只输出"将删除的清单"；
// - 第 4 步先备份 PKI，备份失败立即中止，绝不继续删除；
// - 每个会改主机的动作都走可注入接口，真实实现按平台编译
// （linux 真实执行，其他平台明确报错），因此本包在 darwin 上也能完整测试。
package reset
