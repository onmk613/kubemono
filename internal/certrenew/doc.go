// Package certrenew 实现证书续期。
// 语义：
// - 续期只重签**叶子证书**，CA 与 ServiceAccount 密钥不变（不支持 CA 轮换）；
// - 默认只重签已进入续期窗口（默认 30 天）或已过期的证书，`--all` 重签全部；
// - 重签保留原证书的主题、SAN 与扩展密钥用途，并重新生成私钥
// （SAN 的补齐由 `certs` phase 负责）；
// - 重签后重启受影响的组件（注入 Restarter）；kubelet 客户端证书依靠
// TLS bootstrap 的自动轮换，不在本包处理，只在报告里说明；
// - `kubemono cert renew` 只续期本机：master 逐台手工执行；
// worker 上的 kube-router 使用 ServiceAccount token，不需要证书。
package certrenew
