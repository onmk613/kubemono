package utils

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"
)

// ErrCanceled 是操作者明确拒绝破坏性操作时返回的共享错误。
var ErrCanceled = errors.New("已取消")

// WriteJSON 把值渲染为带缩进的 JSON。
func WriteJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// WriteYAML 把值渲染为 YAML 写入 w。
func WriteYAML(w io.Writer, v any) error {
	data, err := yaml.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

// YAMLWriter 由需要定制 YAML 序列化的类型实现（如 cluster.yaml 的文档序输出）。
// sigs.k8s.io/yaml 走 JSON 再转 YAML 会把字段排成字母序，破坏面向人的阅读顺序。
type YAMLWriter interface {
	WriteYAMLTo(w io.Writer) error
}

// Print 按 --output 渲染命令结果：json/yaml 输出机器可读的 machine，
// table/空值交给 human。所有命令共用这一处分发，新增命令不必再抄一份 switch。
func Print(w io.Writer, format string, machine any, human func(io.Writer) error) error {
	switch format {
	case "json":
		return WriteJSON(w, machine)
	case "yaml":
		if m, ok := machine.(YAMLWriter); ok {
			return m.WriteYAMLTo(w)
		}
		return WriteYAML(w, machine)
	case "", "table":
		if human == nil {
			return nil
		}
		return human(w)
	default:
		// 文案与 root 的 PersistentPreRunE 前置校验保持一致：那里是用户实际
		// 看到的报错，这里是兜底；两处不一致会让同一错误在不同入口显示不同。
		return fmt.Errorf("unsupported --output %q: expected table, json or yaml", format)
	}
}

// RequireSubcommand 报告缺少子命令，并给出可执行的示例。
func RequireSubcommand(cmd *cobra.Command, example string) error {
	_ = cmd.Help()
	return fmt.Errorf("%s 需要指定子命令，例如 %s", cmd.CommandPath(), example)
}

// ConfirmOrCancel 是破坏性命令共用的确认入口：--assume-yes/--dry-run 直接放行，
// 交互确认被拒绝时统一返回 ErrCanceled，Confirm 自身出错（如 stdin 不可读）
// 原样返回。reset/cert renew 共用这一入口，避免复制走样。
func ConfirmOrCancel(in io.Reader, out io.Writer, force, dryRun bool, prompt string) error {
	if force || dryRun {
		return nil
	}
	ok, err := Confirm(in, out, prompt, false)
	if err != nil {
		return err
	}
	if !ok {
		return ErrCanceled
	}
	return nil
}

// Confirm 请求操作者批准破坏性操作。
// 非交互输入（管道）下默认拒绝：破坏性操作不应该因为 stdin 关闭而被“同意”。
// 拒绝时返回 (false, nil)，由调用方统一返回 ErrCanceled——这样 18 个命令里的
// “用户拒绝”分支才真正生效，退出信息也是中文的“已取消”。
func Confirm(in io.Reader, out io.Writer, prompt string, assumeYes bool) (bool, error) {
	if assumeYes {
		return true, nil
	}
	_, _ = fmt.Fprintf(out, "%s [y/N]: ", prompt)
	var answer string
	if _, err := fmt.Fscanln(in, &answer); err != nil {
		return false, ErrCanceled
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "y", "yes":
		return true, nil
	}
	return false, nil
}

// Group 构造只做分组的父命令：不带子命令时给出可执行的下一步提示。
// 所有命令组共用这一份，避免每个 New 都抄一遍 Args/RunE/AddCommand。
func Group(use, short, hint string, children ...*cobra.Command) *cobra.Command {
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return RequireSubcommand(cmd, hint)
		},
	}
	cmd.AddCommand(children...)
	return cmd
}
