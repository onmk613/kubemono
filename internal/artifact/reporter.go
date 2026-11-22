package artifact

// Reporter 在导入/打包过程中接收人类可读的进度行。
type Reporter func(format string, args ...any)

// Reportf 在 Reporter 非 nil 时转发进度行；nil 表示静默。
func (r Reporter) Reportf(format string, args ...any) {
	if r != nil {
		r(format, args...)
	}
}
