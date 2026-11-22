package version_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"kubemono/internal/bom"
	"kubemono/internal/cli/clitest"
)

type textComponent struct {
	name    string
	version string
}

// TestVersionTextCanonicalOrder 检查文本输出沿用 BOM 的规范组件顺序。
func TestVersionTextCanonicalOrder(t *testing.T) {
	out, err := clitest.ExecuteRoot(t, "version")
	if err != nil {
		t.Fatalf("version: %v\n%s", err, out)
	}
	b := bom.MustLoad()
	got := textComponents(t, out)
	names := make([]string, 0, len(got))
	for _, c := range got {
		names = append(names, c.name)
		if c.version != b.ComponentVersion(c.name) {
			t.Fatalf("%s 的版本 = %q, want %q", c.name, c.version, b.ComponentVersion(c.name))
		}
	}
	if want := b.Names(); !slices.Equal(names, want) {
		t.Fatalf("组件顺序 = %v, want %v", names, want)
	}
}

// TestVersionJSONComponents 检查 JSON 输出仍携带完整的组件版本对象。
func TestVersionJSONComponents(t *testing.T) {
	out, err := clitest.ExecuteRoot(t, "version", "--output", "json")
	if err != nil {
		t.Fatalf("version --output json: %v\n%s", err, out)
	}
	var payload struct {
		Components map[string]string `json:"components"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("version --output json 不是合法 JSON: %v\n%s", err, out)
	}
	b := bom.MustLoad()
	if len(payload.Components) != len(b.Components) {
		t.Fatalf("components 数量 = %d, want %d", len(payload.Components), len(b.Components))
	}
	for _, name := range b.Names() {
		if got, want := payload.Components[name], b.ComponentVersion(name); got != want {
			t.Fatalf("components[%q] = %q, want %q", name, got, want)
		}
	}
}

// textComponents 解析 "Pinned component versions:" 之后的组件行。
func textComponents(t *testing.T, out string) []textComponent {
	t.Helper()
	lines := strings.Split(out, "\n")
	start := -1
	for i, line := range lines {
		if strings.TrimSpace(line) == "Pinned component versions:" {
			start = i + 1
			break
		}
	}
	if start < 0 {
		t.Fatalf("输出缺少组件版本标题:\n%s", out)
	}
	var components []textComponent
	for _, line := range lines[start:] {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Fatalf("无法解析组件行 %q:\n%s", line, out)
		}
		components = append(components, textComponent{name: fields[0], version: fields[1]})
	}
	if len(components) == 0 {
		t.Fatalf("没有解析到组件:\n%s", out)
	}
	return components
}
