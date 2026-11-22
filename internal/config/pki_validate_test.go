package config

import "testing"

// TestValidateExternalCAPairs 固定三个信任域的外部 CA 必须 cert/key 成对出现。
func TestValidateExternalCAPairs(t *testing.T) {
	cases := []struct {
		name string
		path string
		mut  func(*Cluster)
	}{
		{"kubernetes", "controlPlane.pki.externalCA", func(c *Cluster) {
			c.ControlPlane.PKI.ExternalCA.CertFile = "/pki/ca.crt"
		}},
		{"front-proxy", "controlPlane.pki.externalFrontProxyCA", func(c *Cluster) {
			c.ControlPlane.PKI.ExternalFrontProxyCA.KeyFile = "/pki/front-proxy-ca.key"
		}},
		{"etcd", "controlPlane.pki.externalEtcdCA", func(c *Cluster) {
			c.ControlPlane.PKI.ExternalEtcdCA.CertFile = "/pki/etcd/ca.crt"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := DefaultCluster()
			tc.mut(c)
			res := Validate(c)
			if res.OK() {
				t.Fatalf("%s 只给一半文件时必须被拒绝", tc.name)
			}
			found := false
			for _, e := range res.Errors {
				if e.Field == tc.path {
					found = true
				}
			}
			if !found {
				t.Fatalf("错误应指向 %s：%v", tc.path, res.Err())
			}
		})
	}
}
