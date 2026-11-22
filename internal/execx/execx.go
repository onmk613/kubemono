// Package execx 是 kubemono 唯一的宿主机命令执行入口。
// 真正的实现只有一个（linux 用 os/exec，其他平台明确报错），各业务包通过
// Runner 接口注入内存 fake 做单元测试。这样不需要在每个包重复一份 exec +
// 一个非 linux stub，也不会出现“某个包的 stub 忘了拒绝执行”的漏洞。
package execx

import "context"

// Runner 执行一条宿主机命令并返回合并输出。
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// Default 返回当前平台的生产执行器。
func Default() Runner { return platformRunner{} }
