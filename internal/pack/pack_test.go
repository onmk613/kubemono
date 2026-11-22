package pack

import (
	"context"
	"testing"

	"kubemono/internal/bom"
)

func TestParseArchSpec(t *testing.T) {
	cases := []struct {
		in       string
		wantOS   string
		wantArch []string
		wantErr  bool
	}{
		{"", DefaultOS, []string{DefaultArch}, false},
		{"amd64", "linux", []string{"amd64"}, false},
		{"linux/amd64", "linux", []string{"amd64"}, false},
		{"linux/arm64", "linux", []string{"arm64"}, false},
		{"amd64,arm64", "linux", []string{"amd64", "arm64"}, false},
		{"linux/amd64,linux/arm64", "linux", []string{"amd64", "arm64"}, false},
		{"amd64,amd64", "linux", []string{"amd64"}, false},
		{"/amd64", "linux", []string{"amd64"}, false},
	}
	for _, tc := range cases {
		osName, archs, err := ParseArchSpec(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseArchSpec(%q) 期望报错", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseArchSpec(%q): %v", tc.in, err)
			continue
		}
		if osName != tc.wantOS {
			t.Errorf("ParseArchSpec(%q) os = %s, want %s", tc.in, osName, tc.wantOS)
		}
		if len(archs) != len(tc.wantArch) {
			t.Errorf("ParseArchSpec(%q) archs = %v, want %v", tc.in, archs, tc.wantArch)
			continue
		}
		for i := range archs {
			if archs[i] != tc.wantArch[i] {
				t.Errorf("ParseArchSpec(%q) archs = %v, want %v", tc.in, archs, tc.wantArch)
				break
			}
		}
	}
}

func TestRunRejectsUnknownArch(t *testing.T) {
	if _, err := Run(context.Background(), Options{Archs: []string{"s390x"}}); err == nil {
		t.Fatal("BOM 不支持的架构必须报错")
	}
}

// TestRunOfflineRequiresCache 确认 --offline 不会悄悄联网：缓存缺失时必须失败。
func TestRunOfflineRequiresCache(t *testing.T) {
	b, err := bom.Parse([]byte(testBOM))
	if err != nil {
		t.Fatal(err)
	}
	_, err = Run(context.Background(), Options{
		BOM:       b,
		CacheRoot: t.TempDir(),
		OutDir:    t.TempDir(),
		Archs:     []string{"amd64"},
		Offline:   true,
	})
	if err == nil {
		t.Fatal("--offline 且缓存缺失时必须报错")
	}
}

func TestRunRejectsOfflineRefresh(t *testing.T) {
	if _, err := Run(context.Background(), Options{Offline: true, Refresh: true, OutDir: t.TempDir(), CacheRoot: t.TempDir()}); err == nil {
		t.Fatal("--offline 与 --refresh 同时使用必须报错")
	}
}
