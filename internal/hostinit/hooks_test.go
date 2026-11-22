package hostinit

import (
	"context"
	"strings"
	"testing"
)

func TestRunHooks(t *testing.T) {
	if err := RunHooks(context.Background(), []string{"", "true"}, nil); err != nil {
		t.Fatalf("成功钩子: %v", err)
	}
	err := RunHooks(context.Background(), []string{"echo boom >&2; exit 7"}, nil)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("失败钩子应包含 stderr，got %v", err)
	}
}
