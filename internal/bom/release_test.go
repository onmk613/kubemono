package bom

import "testing"

func TestParseRelease(t *testing.T) {
	valid := []struct {
		raw        string
		kubernetes string
		patch      int
	}{
		{"v1.37.1-km.1", "v1.37.1", 1},
		{"v1.37.10-km.2", "v1.37.10", 2},
		{"v1.37.1", "v1.37.1", 0},
		{"v1.37.1-alpha.1-km.1", "v1.37.1-alpha.1", 1},
	}
	for _, tc := range valid {
		r, err := ParseRelease(tc.raw)
		if err != nil {
			t.Fatalf("ParseRelease(%q): %v", tc.raw, err)
		}
		if r.Kubernetes != tc.kubernetes || r.Patch != tc.patch || r.String() != tc.raw {
			t.Fatalf("ParseRelease(%q) = %+v (String=%q), want kubernetes=%q patch=%d",
				tc.raw, r, r.String(), tc.kubernetes, tc.patch)
		}
	}

	invalid := []string{
		"", "1.37.1", "v", "v1", "v1.", "v1..2", "v1.2.3.4", "v1.31",
		"v01.2.3", "v1.02.3", "v1.37.1-km", "v1.37.1-km.0",
		"v1.37.1-km.1.2", "v1.37.1-alpha..1-km.1",
	}
	for _, raw := range invalid {
		if r, err := ParseRelease(raw); err == nil {
			t.Fatalf("ParseRelease(%q) succeeded: %+v", raw, r)
		}
	}
}
