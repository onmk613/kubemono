package config

import "testing"

func TestValidateRejectsInvalidHostName(t *testing.T) {
	c := DefaultCluster()
	c.Cluster.Hosts = append(c.Cluster.Hosts, Host{
		Name:    "Bad_Name",
		Address: "10.0.0.99",
		Roles:   []Role{RoleWorker},
	})
	res := Validate(c)
	if res.OK() {
		t.Fatal("非法 host name（下划线/大写）必须被拒绝")
	}
	found := false
	for _, e := range res.Errors {
		if e.Field == "cluster.hosts[1].name" {
			found = true
		}
	}
	if !found {
		t.Fatalf("错误应指向 cluster.hosts[1].name: %v", res.Err())
	}
}

func TestHostNodeNamePolicies(t *testing.T) {
	h := Host{Name: "m1", Address: "10.0.0.1"}
	if got := h.EffectiveNodeName("node-a"); got != "node-a" {
		t.Fatalf("default hostname policy = %q, want node-a", got)
	}
	if got := h.EffectiveNodeName(""); got != "m1" {
		t.Fatalf("hostname fallback = %q, want m1", got)
	}
	h.NodeName = NodeNameFromAddress
	if got := h.EffectiveNodeName("node-a"); got != "10.0.0.1" {
		t.Fatalf("address policy = %q, want 10.0.0.1", got)
	}
	h.NodeName = "m1.example.com"
	if got := h.EffectiveNodeName("node-a"); got != "m1.example.com" {
		t.Fatalf("explicit policy = %q, want m1.example.com", got)
	}
}

func TestValidateNodeName(t *testing.T) {
	c := DefaultCluster()
	c.Cluster.Hosts[0].NodeName = "Bad_Name"
	if res := Validate(c); res.OK() {
		t.Fatal("非法 nodeName 必须被拒绝")
	}

	c = DefaultCluster()
	c.Cluster.Hosts[0].NodeName = "m1"
	c.Cluster.Hosts = append(c.Cluster.Hosts, Host{
		Name:     "m2",
		NodeName: "m1",
		Address:  "10.0.0.2",
		Roles:    []Role{RoleMaster},
	})
	res := Validate(c)
	if res.OK() {
		t.Fatal("重复的 nodeName 必须被拒绝")
	}
}
