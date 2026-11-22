package hostinit

import "context"

// apply 是测试用的默认策略包装：生产代码直接调用 ApplyWithOptions。
func apply(ctx context.Context, steps []Step, ap Applier, journal *Journal) error {
	return ApplyWithOptions(ctx, steps, ap, journal, ApplyOptions{})
}
