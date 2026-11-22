package pki

import (
	"reflect"
	"sort"
	"testing"

	"kubemono/internal/constants"
)

// TestCatalogueInvariants 固定证书表的结构不变量：唯一 ID/文件名、CA 存在、
// kubeconfig 与 certs phase 的归属不重叠。
func TestCatalogueInvariants(t *testing.T) {
	c := DefaultCatalogue
	if len(c.Authorities) != 3 {
		t.Fatalf("authorities = %d，期望 3", len(c.Authorities))
	}
	if len(c.Leaves) != 12 {
		t.Fatalf("leaves = %d，期望 12", len(c.Leaves))
	}
	ids := map[CertID]bool{}
	certFiles := map[string]bool{}
	keyFiles := map[string]bool{}
	for _, l := range c.Leaves {
		if ids[l.ID] {
			t.Errorf("重复 CertID %q", l.ID)
		}
		ids[l.ID] = true
		if l.CertFile == "" || l.KeyFile == "" {
			t.Errorf("%s 缺少文件名", l.ID)
		}
		if certFiles[l.CertFile] {
			t.Errorf("重复证书文件名 %q", l.CertFile)
		}
		certFiles[l.CertFile] = true
		if keyFiles[l.KeyFile] {
			t.Errorf("重复私钥文件名 %q", l.KeyFile)
		}
		keyFiles[l.KeyFile] = true
		if _, ok := c.Authority(l.CA); !ok {
			t.Errorf("%s 引用了不存在的 CA %q", l.ID, l.CA)
		}
		if len(l.Usages) == 0 {
			t.Errorf("%s 没有 EKU", l.ID)
		}
	}
	for _, a := range c.Authorities {
		if a.CertFile == "" || a.KeyFile == "" {
			t.Errorf("CA %s 缺少文件名", a.ID)
		}
	}
	if got := len(c.KubeconfigLeaves()); got != 5 {
		t.Fatalf("kubeconfig leaves = %d，期望 5", got)
	}
}

// TestCataloguePlacement 固定各角色的证书集合。
func TestCataloguePlacement(t *testing.T) {
	cases := []struct {
		name string
		tg   Target
		want []CertID
	}{
		{"master", Target{IsMaster: true}, []CertID{CertAPIServer, CertAPIServerKubeletClient, CertFrontProxyClient}},
		{"mixed master", Target{IsMaster: true, IsWorker: true}, []CertID{CertAPIServer, CertAPIServerKubeletClient, CertFrontProxyClient}},
		{"worker", Target{IsWorker: true}, []CertID{CertAPIServer, CertAPIServerKubeletClient, CertFrontProxyClient}},
		{"etcd master", Target{IsMaster: true, RunsEtcd: true}, []CertID{CertAPIServer, CertAPIServerKubeletClient, CertAPIServerEtcdClient, CertFrontProxyClient, CertEtcdServer, CertEtcdPeer, CertEtcdKubemonoClient}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []CertID
			for _, l := range DefaultCatalogue.CertsLeaves(tc.tg) {
				got = append(got, l.ID)
			}
			sort.Slice(got, func(i, j int) bool { return got[i] < got[j] })
			sort.Slice(tc.want, func(i, j int) bool { return tc.want[i] < tc.want[j] })
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("leaves = %v，期望 %v", got, tc.want)
			}
		})
	}
}

// TestLeafSubjects 固定动态 CN/O。
func TestLeafSubjects(t *testing.T) {
	cn, orgs := mustLeaf(t, CertKubeletClient).SubjectFor("m1")
	if cn != "system:node:m1" || !reflect.DeepEqual(orgs, []string{constants.GroupSystemNodes}) {
		t.Fatalf("kubelet subject = %q/%v", cn, orgs)
	}
}

// TestShippedFiles 固定 join 证书包的证书文件集合。
func TestShippedFiles(t *testing.T) {
	files := DefaultCatalogue.ShippedFiles()
	if len(files) != 10 {
		t.Fatalf("shipped cert files = %d，期望 10", len(files))
	}
	byKey := map[string]ShippedFile{}
	for _, f := range files {
		byKey[f.Key] = f
		if f.Mode != CertFileMode && f.Mode != KeyFileMode {
			t.Errorf("%s 权限 = %04o", f.Key, f.Mode)
		}
	}
	for _, key := range []string{
		"pki/ca.crt", "pki/ca.key", "pki/front-proxy-ca.crt", "pki/front-proxy-ca.key",
		"pki/etcd/ca.crt", "pki/etcd/ca.key",
		"pki/apiserver-etcd-client.crt", "pki/apiserver-etcd-client.key",
		"pki/etcd/kubemono-client.crt", "pki/etcd/kubemono-client.key",
	} {
		if _, ok := byKey[key]; !ok {
			t.Errorf("缺少 %s", key)
		}
	}
	if f := byKey["pki/ca.crt"]; !f.Required || f.Optional {
		t.Errorf("ca.crt 应为 required: %+v", f)
	}
	if f := byKey["pki/etcd/ca.crt"]; f.Required || f.Optional {
		t.Errorf("etcd ca 应为 known-only: %+v", f)
	}
	if f := byKey["pki/apiserver-etcd-client.crt"]; f.Required || f.Optional {
		t.Errorf("apiserver-etcd-client 应为 known-only: %+v", f)
	}
	if f := byKey["pki/etcd/kubemono-client.crt"]; f.Required || !f.Optional {
		t.Errorf("kubemono-client 应为 optional: %+v", f)
	}
}

// mustLeaf 按 ID 查找叶子证书；测试内缺失直接失败。
func mustLeaf(t *testing.T, id CertID) Leaf {
	t.Helper()
	for _, l := range DefaultCatalogue.Leaves {
		if l.ID == id {
			return l
		}
	}
	t.Fatalf("catalogue 缺少 %s", id)
	return Leaf{}
}
