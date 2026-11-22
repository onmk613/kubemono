// Package output 提供面向人的进度输出（reporter）。
// 约定：`--output json|yaml` 时进度消息走 stderr（见 Console.SetProgressToStderr），
// stdout 只留机器可读正文，否则 `... -o json | jq` 会被日志打断。
package output

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

// Reporter 接收面向人的进度消息。
type Reporter interface {
	Debugf(format string, args ...any)
	Infof(format string, args ...any)
	Warnf(format string, args ...any)
	Errorf(format string, args ...any)
	// Step 启动一个具名步骤；返回的函数用于结束它。
	Step(name string) func(err error)
	// Result 报告一次检查类操作的结果。
	Result(ok bool, format string, args ...any)
}

// Console 向 stdout/stderr 写消息，可选着色。
type Console struct {
	mu        sync.Mutex
	Out       io.Writer
	Err       io.Writer
	MinLevel  Level
	Color     bool
	prefixLen int
	// progressToStderr 把 Infof/Step/Result 改写到 Err（--output json|yaml）。
	progressToStderr bool
}

// NewConsole 返回使用进程标准流的控制台 reporter。
func NewConsole() *Console {
	return &Console{
		Out:      os.Stdout,
		Err:      os.Stderr,
		MinLevel: LevelInfo,
		Color:    isTerminal(os.Stdout),
	}
}

// SetVerbose 启用调试消息。
func (c *Console) SetVerbose(v bool) {
	if v {
		c.MinLevel = LevelDebug
	}
}

// SetProgressToStderr 把面向人的进度消息从 stdout 改到 stderr。
// `--output json|yaml` 时 stdout 只应留下机器可读的正文；进度日志混进去会让
// `kubemono ... -o json | jq` 直接失败。
func (c *Console) SetProgressToStderr(on bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.progressToStderr = on
}

// progressWriter 返回面向人的进度消息应写入的流。
func (c *Console) progressWriter() io.Writer {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.progressToStderr && c.Err != nil {
		return c.Err
	}
	return c.Out
}

// ProgressRedirected 报告进度是否已改写到 stderr。
// 调用方据此把其它面向人的正文（例如安装计划）也写到 stderr，保证 stdout 干净。
func (c *Console) ProgressRedirected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.progressToStderr && c.Err != nil
}

// Discard 返回丢弃所有消息的 reporter
func Discard() Reporter { return discardReporter{} }

type discardReporter struct{}

func (discardReporter) Debugf(string, ...any)       {}
func (discardReporter) Infof(string, ...any)        {}
func (discardReporter) Warnf(string, ...any)        {}
func (discardReporter) Errorf(string, ...any)       {}
func (discardReporter) Step(string) func(error)     { return func(error) {} }
func (discardReporter) Result(bool, string, ...any) {}

func (c *Console) write(w io.Writer, level Level, marker, format string, args ...any) {
	if level < c.MinLevel {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	msg := fmt.Sprintf(format, args...)
	if c.Color {
		marker = colour(level, marker)
	}
	if strings.TrimSpace(marker) == "" {
		_, _ = fmt.Fprintln(w, msg)
		return
	}
	_, _ = fmt.Fprintf(w, "%s %s\n", marker, msg)
}

func (c *Console) Debugf(format string, args ...any) {
	c.write(c.progressWriter(), LevelDebug, "·", format, args...)
}

func (c *Console) Infof(format string, args ...any) {
	c.write(c.progressWriter(), LevelInfo, "→", format, args...)
}

func (c *Console) Warnf(format string, args ...any) {
	c.write(c.Err, LevelWarn, "warning:", format, args...)
}

func (c *Console) Errorf(format string, args ...any) {
	c.write(c.Err, LevelError, "error:", format, args...)
}

// Step 报告一个具名步骤的开始与结束。
func (c *Console) Step(name string) func(err error) {
	c.mu.Lock()
	c.prefixLen++
	indent := strings.Repeat("  ", c.prefixLen-1)
	c.mu.Unlock()
	c.write(c.progressWriter(), LevelInfo, indent+"→", "%s", name)
	return func(err error) {
		c.mu.Lock()
		c.prefixLen--
		c.mu.Unlock()
		if err != nil {
			c.write(c.Err, LevelWarn, indent+"  ✗", "%s: %v", name, err)
			return
		}
		c.write(c.progressWriter(), LevelDebug, indent+"  ✓", "%s", name)
	}
}

// Result 报告一次检查结果。
func (c *Console) Result(ok bool, format string, args ...any) {
	marker := "[ok]"
	if !ok {
		marker = "[fail]"
	}
	if c.Color {
		if ok {
			marker = "\x1b[32m[ok]\x1b[0m"
		} else {
			marker = "\x1b[31m[fail]\x1b[0m"
		}
	}
	c.write(c.progressWriter(), LevelInfo, marker, format, args...)
}

// colour 用与级别匹配的 ANSI 颜色包裹 marker。
func colour(level Level, marker string) string {
	switch level {
	case LevelWarn:
		return "\x1b[33m" + marker + "\x1b[0m"
	case LevelError:
		return "\x1b[31m" + marker + "\x1b[0m"
	default:
		return marker
	}
}

func isTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
