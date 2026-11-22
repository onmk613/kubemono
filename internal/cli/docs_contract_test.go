package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// findCommand 按命令路径查找子命令。
func findDocCommand(root *cobra.Command, path ...string) *cobra.Command {
	cur := root
	for _, name := range path {
		var next *cobra.Command
		for _, c := range cur.Commands() {
			if c.Name() == name {
				next = c
				break
			}
		}
		if next == nil {
			return nil
		}
		cur = next
	}
	return cur
}

// TestCLIDocsContract 是 README/CLI 漂移的回归闸门：文档里出现的命令必须存在。
func TestCLIDocsContract(t *testing.T) {
	root := NewRootCmd()
	for _, path := range [][]string{
		{"node", "repair"},
		{"config", "default"},
		{"config", "validate"},
		{"reset"},
		{"cert", "renew"},
		{"pki", "issue"},
		{"token", "create"},
		{"status"},
	} {
		if findDocCommand(root, path...) == nil {
			t.Errorf("文档中的命令不存在: kubemono %s", strings.Join(path, " "))
		}
	}
}

// TestReadmeCommandTableMatchesTree 把 README §4「命令一览」当成契约：
// 表里列出的每个顶层命令/子命令都必须存在，反向也必须成立（新增命令要写进表）。
// 只断言几个硬编码字符串守不住这张表；必须双向解析，新增/删除命令都会被拦住。
func TestReadmeCommandTableMatchesTree(t *testing.T) {
	root := NewRootCmd()
	top, subs := readmeCommandTable(t)

	// README → CLI
	for _, name := range top {
		if findDocCommand(root, name) == nil {
			t.Errorf("README 列出的命令不存在: kubemono %s", name)
		}
	}
	for parent, names := range subs {
		for _, name := range names {
			if findDocCommand(root, parent, name) == nil {
				t.Errorf("README 列出的子命令不存在: kubemono %s %s", parent, name)
			}
		}
	}

	// CLI → README（忽略 cobra 自带的 help/completion）
	documented := map[string]bool{}
	for _, name := range top {
		documented[name] = true
	}
	for _, c := range root.Commands() {
		if c.Hidden || c.Name() == "help" || c.Name() == "completion" {
			continue
		}
		if !documented[c.Name()] {
			t.Errorf("命令 %s 没有出现在 README §4 命令表里", c.Name())
		}
	}
}

// readmeCommandTable 解析 README §4 的表格，返回顶层命令与"父命令 → 子命令"。
func readmeCommandTable(t *testing.T) ([]string, map[string][]string) {
	t.Helper()
	path := filepath.Join("..", "..", "README.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 README: %v", err)
	}
	lines := strings.Split(string(data), "\n")
	inSection := false
	var top []string
	subs := map[string][]string{}
	rowRe := regexp.MustCompile(`^\|\s*` + "`" + `([a-z][a-z-]*)` + "`" + `\s*\|\s*(.*?)\s*\|`)
	tickRe := regexp.MustCompile("`([a-z][a-z-]*)`")
	for _, line := range lines {
		if strings.HasPrefix(line, "## 4.") {
			inSection = true
			continue
		}
		if inSection && strings.HasPrefix(line, "## ") {
			break
		}
		if !inSection {
			continue
		}
		m := rowRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		name, cell := m[1], m[2]
		top = append(top, name)
		for _, sm := range tickRe.FindAllStringSubmatch(cell, -1) {
			subs[name] = append(subs[name], sm[1])
		}
	}
	if len(top) == 0 {
		t.Fatal("README §4 命令表为空或格式已变化（无法解析）")
	}
	return top, subs
}
