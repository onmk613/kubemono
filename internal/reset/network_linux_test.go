//go:build linux

package reset

import "testing"

func TestFirstForwardingRuleSkipsChainDefinitions(t *testing.T) {
	tests := []struct {
		name  string
		rules []string
		want  int
	}{
		{
			name: "user chain has -N header",
			rules: []string{
				"-N KUBE-SERVICES",
				"-A KUBE-SERVICES -d 10.96.0.0/12 -j KUBE-SVC-ABC",
				"-A KUBE-SERVICES -j KUBE-NODEPORTS",
			},
			want: 1,
		},
		{
			name: "builtin chain has -P header",
			rules: []string{
				"-P PREROUTING ACCEPT",
				"-A PREROUTING -m comment --comment \"kubernetes service portals\" -j KUBE-SERVICES",
			},
			want: 1,
		},
		{
			name:  "second real rule",
			rules: []string{"-N X", "-A X -j OTHER", "-A X -j KUBE-SVC"},
			want:  2,
		},
		{
			name:  "no match",
			rules: []string{"-N X", "-A X -j OTHER"},
			want:  0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := firstForwardingRule(tc.rules, "KUBE-"); got != tc.want {
				t.Fatalf("firstForwardingRule(%v) = %d, want %d", tc.rules, got, tc.want)
			}
		})
	}
}
