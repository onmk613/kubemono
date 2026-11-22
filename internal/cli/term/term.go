// Package term 提供 CLI 的终端表格与键值渲染。
// 这些是纯展示函数，不属于 phase/业务层；保留在 CLI 侧可以避免
// internal/util/output 同时承担日志与表格两种职责。
package term

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
)

// Table 渲染对齐的表格（左对齐、无边框）。
func Table(headers []string, rows [][]string) string {
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = displayWidth(h)
	}
	for _, row := range rows {
		for i, cell := range row {
			if i < len(widths) && displayWidth(cell) > widths[i] {
				widths[i] = displayWidth(cell)
			}
		}
	}
	var b strings.Builder
	writeRow := func(cells []string) {
		for i := range widths {
			cell := ""
			if i < len(cells) {
				cell = cells[i]
			}
			if i == len(widths)-1 {
				b.WriteString(cell)
			} else {
				b.WriteString(cell)
				b.WriteString(strings.Repeat(" ", widths[i]-displayWidth(cell)+2))
			}
		}
		b.WriteString("\n")
	}
	writeRow(headers)
	seps := make([]string, len(widths))
	for i := range seps {
		seps[i] = strings.Repeat("-", widths[i])
	}
	writeRow(seps)
	for _, row := range rows {
		writeRow(row)
	}
	return b.String()
}

// displayWidth 返回 s 的终端列宽。
// len(string) 数的是字节，因此中文表头/单元格会被算成可见宽度的 3 倍，
// 所有含 CJK 文本的表格都会错位。这里近似 wcwidth：组合字符为 0，
// 东亚宽/全角字符与 emoji 为 2。
func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		w += runeWidth(r)
	}
	return w
}

var wideRanges = [][2]rune{
	{0x1100, 0x115f},
	{0x2329, 0x232a},
	{0x2e80, 0xa4cf},
	{0xac00, 0xd7a3},
	{0xf900, 0xfaff},
	{0xfe10, 0xfe19},
	{0xfe30, 0xfe6f},
	{0xff00, 0xff60},
	{0xffe0, 0xffe6},
	{0x1f300, 0x1f64f},
	{0x1f900, 0x1f9ff},
	{0x20000, 0x3fffd},
}

func runeWidth(r rune) int {
	if r == 0 || r < 32 || (r >= 0x7f && r < 0xa0) {
		return 0
	}
	if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) || r == 0x200d {
		return 0
	}
	for _, rg := range wideRanges {
		if r >= rg[0] && r <= rg[1] {
			return 2
		}
	}
	return 1
}

// KeyValues 渲染确定性的 "key: value" 列表。
func KeyValues(pairs map[string]string) string {
	keys := make([]string, 0, len(pairs))
	for k := range pairs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%-28s %s\n", k+":", pairs[k])
	}
	return b.String()
}

// KeyValue 是一个保持调用方顺序的键/值对。
type KeyValue struct {
	Key   string
	Value string
}

// KeyValuesOrdered 按给定顺序渲染对齐的键/值列表。
// 输出是 bundle/version 等命令的既有契约（键宽按整表最大值对齐、值前固定两列）。
func KeyValuesOrdered(pairs []KeyValue) string {
	width := 0
	for _, p := range pairs {
		if len(p.Key) > width {
			width = len(p.Key)
		}
	}
	var b strings.Builder
	for _, p := range pairs {
		fmt.Fprintf(&b, "  %-*s  %s\n", width, p.Key, p.Value)
	}
	return b.String()
}

// KeyValuesAligned 渲染按键名排序的键值列表。
func KeyValuesAligned(pairs map[string]string) string {
	keys := make([]string, 0, len(pairs))
	for k := range pairs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	ordered := make([]KeyValue, 0, len(keys))
	for _, k := range keys {
		ordered = append(ordered, KeyValue{Key: k, Value: pairs[k]})
	}
	return KeyValuesOrdered(ordered)
}

// YesNo 把布尔值渲染为“是/否”，供表格输出复用。
func YesNo(v bool) string {
	if v {
		return "是"
	}
	return "否"
}
