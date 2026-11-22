package identity

import (
	"net"
	"net/netip"
	"testing"
)

func TestParseInterfaceAddr(t *testing.T) {
	tests := []struct {
		in       string
		want     string
		wantFail bool
	}{
		{in: "10.0.0.11/24", want: "10.0.0.11/24"},
		{in: "2001:db8::1/64", want: "2001:db8::1/64"},
		{in: "fe80::1%eth0/64", want: "fe80::1/64"},
		{in: "fe80::1%eth0", want: "fe80::1/128"},
		{in: "10.0.0.11", want: "10.0.0.11/32"},
		{in: "not-an-address", wantFail: true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, ok := parseInterfaceAddr(fakeAddr(tc.in))
			if tc.wantFail {
				if ok {
					t.Fatalf("parseInterfaceAddr(%q) = %v, want failure", tc.in, got)
				}
				return
			}
			if !ok {
				t.Fatalf("parseInterfaceAddr(%q) failed, want %s", tc.in, tc.want)
			}
			if got.String() != tc.want {
				t.Errorf("parseInterfaceAddr(%q) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}

// fakeAddr 把字符串适配成 net.Addr。
type fakeAddr string

func (a fakeAddr) Network() string { return "ip" }
func (a fakeAddr) String() string  { return string(a) }

func TestParseAddress(t *testing.T) {
	tests := []struct {
		in   string
		want string
		ok   bool
	}{
		{in: "10.0.0.1", want: "10.0.0.1", ok: true},
		{in: " 10.0.0.1 ", want: "10.0.0.1", ok: true},
		{in: "10.0.0.1/24", want: "10.0.0.1", ok: true},
		{in: "2001:db8::1", want: "2001:db8::1", ok: true},
		{in: "fe80::1%eth0", want: "fe80::1", ok: true},
		{in: "::ffff:10.0.0.1", want: "10.0.0.1", ok: true},
		{in: "", ok: false},
		{in: "host.example.com", ok: false},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, ok := parseAddress(tc.in)
			if ok != tc.ok {
				t.Fatalf("parseAddress(%q) ok = %v, want %v", tc.in, ok, tc.ok)
			}
			if ok && got.String() != tc.want {
				t.Errorf("parseAddress(%q) = %s, want %s", tc.in, got, tc.want)
			}
		})
	}
}

func TestIsExcludedAddress(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{in: "127.0.0.1/8", want: true},
		{in: "::1/128", want: true},
		{in: "169.254.0.1/16", want: true},
		{in: "fe80::1/64", want: true},
		{in: "10.0.0.1/24", want: false},
		{in: "2001:db8::1/64", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			if got := isExcludedAddress(netip.MustParsePrefix(tc.in)); got != tc.want {
				t.Errorf("isExcludedAddress(%s) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestNormalizeAddress(t *testing.T) {
	tests := map[string]string{
		"10.0.0.1":       "10.0.0.1",
		" 10.0.0.1 ":     "10.0.0.1",
		"10.0.0.1/24":    "10.0.0.1",
		"2001:db8::1":    "2001:db8::1",
		"weird-hostname": "weird-hostname",
		"":               "",
	}
	for in, want := range tests {
		if got := normalizeAddress(in); got != want {
			t.Errorf("normalizeAddress(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestInterfaceHelpers(t *testing.T) {
	i := Interface{Name: "eth0", Flags: []string{FlagUp, FlagMulticast}}
	if !i.IsUp() || !i.HasFlag(FlagMulticast) {
		t.Errorf("IsUp/HasFlag = false, want true for %+v", i)
	}
	if i.IsLoopback() {
		t.Errorf("IsLoopback() = true, want false")
	}
	if i.HasFlag(FlagLoopback) {
		t.Errorf("HasFlag(loopback) = true, want false")
	}
	if flags := interfaceFlags(net.FlagUp | net.FlagLoopback | net.FlagPointToPoint | net.FlagMulticast); len(flags) != 4 {
		t.Errorf("interfaceFlags() = %v, want all four flags", flags)
	}
	if flags := interfaceFlags(0); len(flags) != 0 {
		t.Errorf("interfaceFlags(0) = %v, want none", flags)
	}
}

func TestHostname(t *testing.T) {
	name, err := Hostname()
	if err != nil {
		t.Skipf("Hostname() error = %v; skipping", err)
	}
	if name == "" {
		t.Errorf("Hostname() = empty, want a non-empty host name")
	}
}
