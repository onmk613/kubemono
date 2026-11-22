package configcmd_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"kubemono/internal/cli/clitest"
)

const componentVersionsClusterTemplate = `cluster:
  hosts:
    - {name: m1, address: 10.0.0.1, roles: [master]}
images:
  componentVersions:
  %s
`

func writeComponentVersionsCluster(t *testing.T, versions string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cluster.yaml")
	content := strings.Replace(componentVersionsClusterTemplate, "%s", versions, 1)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConfigValidateComponentVersions(t *testing.T) {
	valid := writeComponentVersionsCluster(t, "    coredns: v1.14.7")
	if out, err := clitest.ExecuteRoot(t, "config", "validate", "-f", valid); err != nil {
		t.Fatalf("合法覆盖被拒绝: %v\n%s", err, out)
	}

	bad := map[string]string{
		"k8s":      "    kube-apiserver: v1.37.2",
		"kubemono": "    kubemono: v1.37.1-km.2",
		"unknown":  "    not-a-component: v1.0.0",
	}
	for name, versions := range bad {
		path := writeComponentVersionsCluster(t, versions)
		if out, err := clitest.ExecuteRoot(t, "config", "validate", "-f", path); err == nil {
			t.Errorf("%s: 应拒绝覆盖\n%s", name, out)
		}
	}
}
