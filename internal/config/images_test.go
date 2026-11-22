package config

import "testing"

func TestImageOverrides(t *testing.T) {
	c := DefaultCluster()
	c.Images.Components = map[string]string{
		"coredns": "registry.example.com/kubemono/coredns/coredns:v1.14.6",
		"pause":   "registry.example.com/kubemono/pause:3.10.2",
	}
	got := c.ImageOverrides()
	if len(got) != 2 || got["coredns"] == "" || got["pause"] == "" {
		t.Fatalf("ImageOverrides = %v", got)
	}
	var nilCluster *Cluster
	if got := nilCluster.ImageOverrides(); got != nil {
		t.Fatalf("nil cluster = %v", got)
	}
}

func TestImageOverridesSkipsEmptyImage(t *testing.T) {
	c := DefaultCluster()
	c.Images.Components = map[string]string{"coredns": ""}
	if got := c.ImageOverrides(); got != nil {
		t.Fatalf("空 image 不应产生覆盖: %v", got)
	}
}

func TestLoadClusterFlattenedImageOverrides(t *testing.T) {
	const base = `cluster:
  hosts:
    - {name: m1, address: 10.0.0.1, roles: [master]}
images:
  components:
    coredns: registry.example.com/kubemono/coredns/coredns:v1.14.6
`
	c, err := LoadCluster([]byte(base))
	if err != nil {
		t.Fatalf("LoadCluster: %v", err)
	}
	if got, want := c.Images.Components["coredns"], "registry.example.com/kubemono/coredns/coredns:v1.14.6"; got != want {
		t.Fatalf("components[coredns] = %q, want %q", got, want)
	}

	nested := `cluster:
  hosts:
    - {name: m1, address: 10.0.0.1, roles: [master]}
images:
  components:
    coredns:
      image: registry.example.com/kubemono/coredns/coredns:v1.14.6
`
	if _, err := LoadCluster([]byte(nested)); err == nil {
		t.Fatal("嵌套的 components.<name>.image 必须被拒绝")
	}
}
