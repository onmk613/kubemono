// Package stringsx 收纳多个包共用的字符串切片小工具。
//
// 存在的理由很具体：`去空白 + 去空串 + 去重 + 排序` 这段逻辑在 network、
// render、pki 三个包里各写了一份逐字相同的实现，改一处漏两处。渲染结果的
// 确定性依赖这个语义（顺序稳定 = 指纹稳定 = 不产生无谓的写入/滚动），
// 所以只留一份。
package stringsx

import (
	"sort"
	"strings"
)

// DedupSorted 去掉首尾空白与空串、去重并排序。
// 结果确定（同输入同输出），可直接用于渲染与指纹计算。
func DedupSorted(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// SetEqual 报告两个字符串切片是否包含完全相同的元素（忽略顺序，重复项按
// 多重集合处理）。用于比较"证书组织/组"这类语义上是集合、但顺序不稳定的值。
func SetEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	count := make(map[string]int, len(a))
	for _, s := range a {
		count[s]++
	}
	for _, s := range b {
		count[s]--
		if count[s] < 0 {
			return false
		}
	}
	return true
}
