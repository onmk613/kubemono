//go:build !linux

package hostinit

import (
	"context"
	"fmt"
	"strings"
)

// Run 在非 linux 平台不可用。
func (osCollector) Run(ctx context.Context, name string, args ...string) (string, error) {
	return "", fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), ErrUnsupported)
}

// StatFS 在非 linux 平台不可用。
func (osCollector) StatFS(path string) (uint64, uint64, error) {
	return 0, 0, fmt.Errorf("statfs %s: %w", path, ErrUnsupported)
}
