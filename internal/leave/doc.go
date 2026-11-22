// Package leave 实现节点退出与集群侧清理。
//
//	Leave 在待退出节点本机执行：cordon → 删除 Node 对象
//	→ 移除 etcd 成员（校验法定人数）→ 本机 reset。
//
// 驱逐工作负载由运维用 `kubectl drain` 完成：kubemono 只 cordon，不重复实现 drain。
// 顺序是刻意的：Node 对象先删，避免 kubelet 在清理过程中重新注册；
// etcd 成员最后摘，保证前面步骤仍然能通过 apiserver 完成。
// 所有集群侧操作都只经过 apiserver，并要求调用方提供 admin 凭据
// （缺少时返回 ErrNoClient，错误文本直接告诉运维人员需要什么）。
//
// 配置是本地文件：节点行由运维从 cluster.yaml 删除，
// 本包不写入集群内配置副本。
package leave
