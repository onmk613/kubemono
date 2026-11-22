package config

import (
	"encoding/json"
	"testing"
	"time"
)

// TestDurationSupportsDayAndWeekUnits 覆盖 cluster.yaml 中使用的单位
// （certValidity: 87600h、warnBefore: 30d、备份间隔 6h）。
func TestDurationSupportsDayAndWeekUnits(t *testing.T) {
	cases := map[string]time.Duration{
		"30s":    30 * time.Second,
		"30m":    30 * time.Minute,
		"6h":     6 * time.Hour,
		"30d":    30 * 24 * time.Hour,
		"2w":     14 * 24 * time.Hour,
		"87600h": 87600 * time.Hour,
		"1.5d":   36 * time.Hour,
	}
	for in, want := range cases {
		var d Duration
		if err := json.Unmarshal([]byte(`"`+in+`"`), &d); err != nil {
			t.Fatalf("unmarshal %q: %v", in, err)
		}
		if d.D() != want {
			t.Errorf("%q parsed as %s, want %s", in, d.D(), want)
		}
	}
	for _, bad := range []string{"", "abc", "10x", "-"} {
		var d Duration
		if err := d.Set(bad); err == nil && bad != "" {
			t.Errorf("Set(%q) must fail", bad)
		}
	}
}

// TestDurationMarshalsAsString 让配置保持人类可编辑。
func TestDurationMarshalsAsString(t *testing.T) {
	d := Duration(90 * time.Minute)
	data, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `"1h30m0s"` {
		t.Fatalf("marshalled as %s", data)
	}
}
