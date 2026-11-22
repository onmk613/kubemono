package term

import (
	"strings"
	"testing"
)

func TestTableAlignsCJKColumns(t *testing.T) {
	table := Table(
		[]string{"节点", "状态"},
		[][]string{
			{"m1", "正常"},
			{"wq-test2", "异常"},
		},
	)
	lines := strings.Split(strings.TrimRight(table, "\n"), "\n")
	if len(lines) < 4 {
		t.Fatalf("table 行数不足：\n%s", table)
	}
	want := displayWidth(lines[0])
	for i, line := range lines {
		if got := displayWidth(line); got != want {
			t.Fatalf("第 %d 行宽度 %d != 表头宽度 %d（CJK 对齐失败）：\n%s", i, got, want, table)
		}
	}
}

func TestKeyValuesSorted(t *testing.T) {
	got := KeyValues(map[string]string{"b": "2", "a": "1"})
	if strings.Index(got, "a:") > strings.Index(got, "b:") {
		t.Fatalf("KeyValues 未按 key 排序：%q", got)
	}
}
