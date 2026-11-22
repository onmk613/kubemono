package systemd

import (
	"errors"
	"testing"
)

func TestParseActive(t *testing.T) {
	cases := []struct {
		out  string
		err  error
		want bool
		bad  bool
	}{
		{"active\n", nil, true, false},
		{"reloading", nil, true, false},
		{"inactive", errors.New("exit 3"), false, false},
		{"failed", errors.New("exit 3"), false, false},
		{"maintenance", errors.New("exit 3"), false, false},
		{"", errors.New("exec: systemctl not found"), false, true},
	}
	for _, tc := range cases {
		got, err := ParseActive([]byte(tc.out), tc.err)
		if tc.bad {
			if err == nil {
				t.Errorf("ParseActive(%q) = nil error, want error", tc.out)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("ParseActive(%q) = (%v,%v), want (%v,nil)", tc.out, got, err, tc.want)
		}
	}
}

// TestState 固定 hostinit 记录的状态集合与 ParseActive 一致。
func TestState(t *testing.T) {
	cases := []struct {
		out  string
		err  error
		want string
	}{
		{"active\n", nil, "active"},
		{"reloading\n", nil, "reloading"},
		{"activating\n", errors.New("exit 3"), "activating"},
		{"inactive\n", errors.New("exit 3"), "inactive"},
		{"failed\n", errors.New("exit 3"), "failed"},
		{"maintenance\n", errors.New("exit 3"), "maintenance"},
		{"", errors.New("exec: systemctl not found"), "unknown"},
		{"something-new\n", nil, "unknown"},
	}
	for _, tc := range cases {
		if got := State([]byte(tc.out)); got != tc.want {
			t.Errorf("State(%q) = %q, want %q", tc.out, got, tc.want)
		}
	}
}
