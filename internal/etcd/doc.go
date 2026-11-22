// Package etcd 管理 etcd 集群的安装期动作：健康检查、成员变更，以及
// stacked / external 两种部署形态的差异收敛。
// # 范围
// 本包只服务于 install/join/leave/reset：成员 add/promote/remove、健康等待、
// 数据目录与 systemd unit 操作。快照备份与离线恢复不在工具内，请使用
// etcdctl / etcdutl（例如 `etcdctl snapshot save`、`etcdutl snapshot restore`）。
// # 依赖边界
// 本包只依赖 go.etcd.io/etcd/client/v3 与标准库；不调用外部 etcdutl 进程。
// # 注入缝
// 所有会改动本机的动作都通过接口注入，真实实现放在 //go:build linux 的文件里，
// 非 linux 平台返回明确错误（开发机是 macOS，测试全部使用 fake）：
// - ServiceManager：systemd 动作（stacked 的 etcd.service）；
// - LocalOps：etcd 数据目录的文件操作。
// # 形态
// Provider 是 表格的唯一分叉点：stacked 允许成员变更；
// external+unmanaged 只做健康检查，成员不变更。
package etcd
