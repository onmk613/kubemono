package config

import "testing"

// TestLoadNodeConfigRejectsUnknownFields 确认严格解析：未知字段直接拒绝。
func TestLoadNodeConfigRejectsUnknownFields(t *testing.T) {
	for name, extra := range map[string]string{
		"unknown": "notARealField: 1",
	} {
		t.Run(name, func(t *testing.T) {
			data := []byte("nodeName: m1\nhostName: m1\n" + extra + "\n")
			if _, err := LoadNodeConfig(data); err == nil {
				t.Fatal("未知字段必须被拒绝")
			}
		})
	}
}
