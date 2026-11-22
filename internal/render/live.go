package render

import (
	"os"
	"sort"
)

// OverlayLive 返回 base，并把每个仍然需要存在的文件的内容与权限替换为磁盘上的实际
// 状态，这样 ComputeChanges 报告的是真实漂移，而不是"快照没有变化"。
// 为什么需要它：本机 desired 快照（Store）只记录"上一次渲染出的内容"，磁盘上的
// 文件可能被运维或第三方工具改过。phase 必须先看 live 再决定要不要落盘，
// 否则"快照没变"会让漂移的配置被静默跳过。
// 语义（kubelet 与 network phase 共用同一份实现）：
// - base 里存在、desired 里不存在的文件 → 保留在结果里，ComputeChanges 会
// 把它判定为 ChangeRemoved（这一代不需要的文件必须删除）；
// - desired 里存在、磁盘上不存在的文件 → 从结果里删掉，ComputeChanges 判定为
// ChangeAdded（新文件不应该触发备份）；
// - 两边都存在的文件 → 内容与权限以磁盘为准，因此漂移会被重写。
func OverlayLive(base, desired *Spec) *Spec {
	out := &Spec{Node: desired.Node}
	if base != nil {
		out.Files = append(out.Files, base.Files...)
	}
	for _, d := range desired.Files {
		live, ok := ReadLive(d)
		if !ok {
			out.Files = removePath(out.Files, d.Path)
			continue
		}
		out.Files = replacePath(out.Files, live)
	}
	out.Sort()
	return out
}

// ReadLive 读取某个期望文件的当前磁盘状态。
// 文件不存在（或不可读）时返回 false：调用方据此判断"这是一个新增文件"。
func ReadLive(d File) (File, bool) {
	data, err := os.ReadFile(d.Path)
	if err != nil {
		return File{}, false
	}
	info, err := os.Stat(d.Path)
	if err != nil {
		return File{}, false
	}
	live := d
	live.Content = data
	live.Mode = uint32(info.Mode().Perm())
	return live, true
}

// SelectComponent 返回某个归属组件名下的文件。
func SelectComponent(spec *Spec, component string) *Spec {
	out := &Spec{Node: spec.Node}
	for _, f := range spec.Files {
		if f.Component == component {
			out.Files = append(out.Files, f)
		}
	}
	sort.Slice(out.Files, func(i, j int) bool { return out.Files[i].Path < out.Files[j].Path })
	return out
}

// SelectComponents 返回快照中属于 desired 所含组件的文件。
// 用途：本机 desired 快照是**整机共享**的（install / node repair 共同维护），
// 单个 phase 想算"这一代不需要、必须删除的文件"时，必须先把快照过滤到自己负责的
// 组件上。直接拿整份快照和自己那份期望状态比较，会把别的组件的文件判成"应删除"并
// 真的删掉（真机实测：network phase 因此删掉了 /etc/kubernetes/manifests/* 与
// kubelet.service，刚装好的集群当场挂掉）。
// 组件清单直接取自 desired，避免每个 phase 手写成分漏项。
func SelectComponents(snapshot, desired *Spec) *Spec {
	if snapshot == nil || desired == nil {
		return nil
	}
	out := &Spec{Node: snapshot.Node}
	for _, component := range desired.Components() {
		part := SelectComponent(snapshot, component)
		out.Files = append(out.Files, part.Files...)
	}
	sort.Slice(out.Files, func(i, j int) bool { return out.Files[i].Path < out.Files[j].Path })
	return out
}

func removePath(files []File, path string) []File {
	out := files[:0]
	for _, f := range files {
		if f.Path != path {
			out = append(out, f)
		}
	}
	return out
}

func replacePath(files []File, f File) []File {
	for i := range files {
		if files[i].Path == f.Path {
			files[i] = f
			return files
		}
	}
	return append(files, f)
}
