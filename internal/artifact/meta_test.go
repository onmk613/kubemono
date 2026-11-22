package artifact

import (
	"testing"
	"time"

	"kubemono/internal/bom"
	"kubemono/internal/util/fsutil"
)

func validMeta(t *testing.T) *Meta {
	t.Helper()
	body := "kubelet"
	return &Meta{
		FormatVersion: FormatVersion,
		Release:       bom.MustLoad().Release,
		Kubernetes:    bom.MustLoad().Kubernetes,
		Arch:          "amd64",
		OS:            "linux",
		Created:       time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC),
		Builder:       "test",
		Files: []FileEntry{
			{
				Path: "bin/k8s/kubelet", SHA256: fsutil.SHA256Bytes([]byte(body)),
				Size: int64(len(body)), Mode: "0755", Component: "kubelet", Category: "k8s",
			},
		},
	}
}

func TestMetaRoundTrip(t *testing.T) {
	m := validMeta(t)
	raw, err := MarshalMeta(m)
	if err != nil {
		t.Fatalf("MarshalMeta: %v", err)
	}
	got, err := ParseMeta(raw)
	if err != nil {
		t.Fatalf("ParseMeta: %v", err)
	}
	if got.Release != m.Release || len(got.Files) != 1 || got.Files[0].Path != "bin/k8s/kubelet" {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if got.FormatVersion != FormatVersion {
		t.Fatalf("formatVersion = %d", got.FormatVersion)
	}
}

func TestMetaRejectsOldFormat(t *testing.T) {
	m := validMeta(t)
	m.FormatVersion = 1
	if err := m.Validate(); err == nil {
		t.Fatal("formatVersion 1 must be rejected")
	}
}

func TestMetaRejectsReservedAndTraversal(t *testing.T) {
	cases := []string{"meta.json", ".complete", "../escape", "/abs", "bin/../x", "bin//x"}
	for _, p := range cases {
		m := validMeta(t)
		m.Files[0].Path = p
		if err := m.Validate(); err == nil {
			t.Errorf("path %q must be rejected", p)
		}
	}
}

func TestMetaRejectsBadDigest(t *testing.T) {
	m := validMeta(t)
	m.Files[0].SHA256 = "sha256:xyz"
	if err := m.Validate(); err == nil {
		t.Fatal("invalid sha256 must be rejected")
	}
}

func TestMetaNormalize(t *testing.T) {
	m := validMeta(t)
	m.Files = append(m.Files, m.Files[0])
	m.Files = append(m.Files, FileEntry{Path: "bin/k8s/aaa", Mode: "0644"})
	m.Images = []ImageEntry{{Reference: "z"}, {Reference: "a"}}
	m.Components = []ComponentEntry{{Name: "z"}, {Name: "a"}}
	m.Normalize()
	if len(m.Files) != 2 {
		t.Fatalf("files = %d, want 2", len(m.Files))
	}
	if m.Files[0].Path != "bin/k8s/aaa" {
		t.Fatalf("files not sorted: %+v", m.Files)
	}
	if m.Images[0].Reference != "a" || m.Components[0].Name != "a" {
		t.Fatalf("images/components not sorted")
	}
}

func TestPlatformCheck(t *testing.T) {
	m := validMeta(t)
	if err := m.CheckPlatform("linux", "amd64"); err != nil {
		t.Fatalf("CheckPlatform: %v", err)
	}
	if err := m.CheckPlatform("darwin", "amd64"); err == nil {
		t.Fatal("OS mismatch must fail")
	}
	if err := m.CheckPlatform("linux", "arm64"); err == nil {
		t.Fatal("arch mismatch must fail")
	}
}
