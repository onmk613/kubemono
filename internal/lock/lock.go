// Package lock 提供跨进程的“同一数据目录只允许一个写操作”锁。
//
// 无守护模型下，并发通常来自 operator 的两次终端、CI 重试、cert timer 与
// node repair 撞车。进程内 mutex 挡不住这些；这里用 flock（文件锁），
// 进程退出/崩溃时内核自动释放，不需要处理 stale lock 文件。
package lock

import "errors"

// ErrBusy 表示另一个 kubemono 进程持有同一把锁。
var ErrBusy = errors.New("another kubemono operation is already running on this data directory")
