package install

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kubemono/internal/artifact"
	"kubemono/internal/bom"
	"kubemono/internal/constants"
)

func writeMeta(t *testing.T, opts *Options, meta *artifact.Meta) {
	t.Helper()
	dir := opts.Paths.ArtifactDir(opts.Release)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := artifact.MarshalMeta(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, artifact.MetaName), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyMaterialsRejectsVersionDrift(t *testing.T) {
	b := bom.MustLoad()
	opts := &Options{
		BOM:     b,
		Release: b.Release,
		Paths:   constants.NewPaths(t.TempDir()),
	}
	meta := &artifact.Meta{
		FormatVersion: artifact.FormatVersion,
		Release:       b.Release,
		Kubernetes:    b.Kubernetes,
		OS:            "linux",
		Arch:          "amd64",
		Components:    []artifact.ComponentEntry{{Name: "kubelet", Version: b.ComponentVersion("kubelet")}},
	}
	writeMeta(t, opts, meta)
	if err := verifyMaterials(opts); err != nil {
		t.Fatalf("一致物料不应报错: %v", err)
	}

	meta.Kubernetes = "v1.30.0"
	writeMeta(t, opts, meta)
	if err := verifyMaterials(opts); err == nil || !strings.Contains(err.Error(), "Kubernetes") {
		t.Fatalf("Kubernetes 漂移必须被拒绝，got %v", err)
	}

	meta.Kubernetes = b.Kubernetes
	meta.Components = []artifact.ComponentEntry{{Name: "kubelet", Version: "v1.30.0"}}
	writeMeta(t, opts, meta)
	if err := verifyMaterials(opts); err == nil || !strings.Contains(err.Error(), "kubelet") {
		t.Fatalf("组件版本漂移必须被拒绝，got %v", err)
	}
}
