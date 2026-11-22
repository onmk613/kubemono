package clusterinit

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/clientcmd"
	tokenapi "k8s.io/cluster-bootstrap/token/api"

	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	"kubemono/internal/discovery"
	"kubemono/internal/pki"
)

// pemCA 生成 PEM 形式的自签 CA 证书。
func pemCA(t *testing.T) []byte {
	t.Helper()
	ca, err := pki.NewCA("kubernetes-ca", "", 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return pki.EncodeCertPEM(ca.Cert)
}

func testCluster() *clusterconfig.Cluster {
	c := &clusterconfig.Cluster{}
	c.Cluster.Hosts = []clusterconfig.Host{
		{Name: "m1", Address: "10.0.0.1", Roles: []clusterconfig.Role{clusterconfig.RoleMaster}},
		{Name: "w1", Address: "10.0.0.11", Roles: []clusterconfig.Role{clusterconfig.RoleWorker}},
	}
	clusterconfig.SetDefaults(c)
	return c
}

func testOptions() Options {
	return Options{
		Cluster:   testCluster(),
		ClusterID: "cid-1234",
		Release:   "v1.31.4-km.1",
	}
}

func TestBuildProducesEveryDocumentedResource(t *testing.T) {
	rs, err := Build(testOptions())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	names := rs.Names()
	want := []string{
		"clusterrole/kubemono:cluster-admins",
		"clusterrolebinding/kubemono:bootstrap:worker",
		"clusterrolebinding/kubemono:bootstrap:master",
		"clusterrolebinding/kubemono:apiserver-kubelet-admin",
		"clusterrolebinding/kubemono:cluster-admins",
		"role/kube-system/kubemono:bootstrap:certs",
		"rolebinding/kube-system/kubemono:bootstrap:certs",
		"configmap/kube-system/kubemono-cluster",
	}
	for _, w := range want {
		found := false
		for _, n := range names {
			if n == w {
				found = true
			}
		}
		if !found {
			t.Errorf("缺少资源 %s（现有：%v）", w, names)
		}
	}
	for _, n := range names {
		if strings.HasPrefix(n, "secret/") {
			t.Errorf("cluster-init 不应再创建 Secret（加入凭证由 token create 负责）: %s", n)
		}
	}
}

func TestClusterStateConfigMapCarriesIdentityAndNetworks(t *testing.T) {
	rs, err := Build(testOptions())
	if err != nil {
		t.Fatal(err)
	}
	cm := rs.ConfigMaps[0]
	if cm.Name != constants.ClusterStateName || cm.Namespace != constants.KubeSystemNamespace {
		t.Fatalf("集群状态对象位置不对: %s/%s", cm.Namespace, cm.Name)
	}
	for key, want := range map[string]string{
		"clusterID":   "cid-1234",
		"release":     "v1.31.4-km.1",
		"podCIDR":     "10.244.0.0/16",
		"serviceCIDR": "10.96.0.0/12",
		"clusterDNS":  "10.96.0.10",
		"dnsDomain":   "cluster.local",
	} {
		if got := cm.Data[key]; got != want {
			t.Errorf("kubemono-cluster.%s = %q, want %q", key, got, want)
		}
	}
}

func TestBuildRejectsMissingIdentity(t *testing.T) {
	opts := testOptions()
	opts.ClusterID = ""
	if _, err := Build(opts); err == nil {
		t.Fatal("缺少 clusterID 必须报错（它是集群身份）")
	}
	opts = testOptions()
	opts.Release = ""
	if _, err := Build(opts); err == nil {
		t.Fatal("缺少 release 必须报错")
	}
	if _, err := Build(Options{}); err == nil {
		t.Fatal("缺少集群配置必须报错")
	}
}

func TestApplyIsIdempotent(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	rs, err := Build(testOptions())
	if err != nil {
		t.Fatal(err)
	}
	done, err := rs.Check(ctx, client)
	if err != nil {
		t.Fatal(err)
	}
	if done {
		t.Fatal("空集群不应报告完成")
	}
	if err := rs.Apply(ctx, client, nil); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	done, err = rs.Check(ctx, client)
	if err != nil || !done {
		t.Fatalf("Apply 后 Check(done=%v err=%v)", done, err)
	}
	// 再应用一次不应报错（对象已存在）。
	if err := rs.Apply(ctx, client, nil); err != nil {
		t.Fatalf("重复 Apply 必须幂等: %v", err)
	}

	// 校验关键对象真实落库。
	if _, err := client.RbacV1().ClusterRoleBindings().Get(ctx, "kubemono:bootstrap:master", metav1.GetOptions{}); err != nil {
		t.Fatalf("master 引导绑定未创建: %v", err)
	}
}

func TestApplyPreservesClusterInfoIdentityAndSignatures(t *testing.T) {
	ctx := context.Background()
	existing := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: constants.ClusterInfoName, Namespace: constants.KubePublicNamespace},
		Data: map[string]string{
			"kubemono-cluster-id":            "cid-1",
			"kubemono-cluster-id-jws-abcdef": "id-sig",
			"jws-kubeconfig-abcdef":          "kc-sig",
		},
	}
	client := fake.NewSimpleClientset(existing)
	opts := testOptions()
	opts.CACertPEM = pemCA(t)
	rs, err := Build(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := rs.Apply(ctx, client, nil); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	cm, err := client.CoreV1().ConfigMaps(constants.KubePublicNamespace).Get(ctx, constants.ClusterInfoName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"kubemono-cluster-id", "kubemono-cluster-id-jws-abcdef", "jws-kubeconfig-abcdef"} {
		if cm.Data[key] == "" {
			t.Fatalf("cluster-info 的 %s 被覆盖丢失: %v", key, cm.Data)
		}
	}
}

func TestApplyRejectsMissingClient(t *testing.T) {
	rs, err := Build(testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := rs.Apply(context.Background(), nil, nil); err == nil {
		t.Fatal("缺少客户端必须报错")
	}
	if _, err := rs.Check(context.Background(), nil); err == nil {
		t.Fatal("缺少客户端必须报错")
	}
}

func TestSummaryAndNamesAreDeterministic(t *testing.T) {
	rs, err := Build(testOptions())
	if err != nil {
		t.Fatal(err)
	}
	first := rs.Names()
	second := rs.Names()
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("Names() 不确定: %v vs %v", first, second)
		}
	}
	if !strings.Contains(rs.Summary(), "clusterrole=2") || strings.Contains(rs.Summary(), "namespace=") {
		t.Fatalf("Summary 不正确: %s", rs.Summary())
	}
}

// TestClusterInfoHasNoPerTokenSignatures 固定 init 的边界：cluster-info 发布
// kubeconfig、release 与 clusterID（不带签名）；每个令牌的 JWS 和集群身份 JWS
// 由 token create 在签发时写入。
func TestClusterInfoHasNoPerTokenSignatures(t *testing.T) {
	opts := testOptions()
	opts.CACertPEM = pemCA(t)
	opts.PublishClusterID = true

	rs, err := Build(opts)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	var ci *corev1.ConfigMap
	for _, cm := range rs.ConfigMaps {
		if cm.Name == constants.ClusterInfoName {
			ci = cm
		}
	}
	if ci == nil {
		t.Fatal("缺少 cluster-info ConfigMap")
	}
	if ci.Namespace != constants.KubePublicNamespace {
		t.Fatalf("cluster-info 必须在 kube-public，实际 %s", ci.Namespace)
	}
	if ci.Data[tokenapi.KubeConfigKey] == "" {
		t.Fatal("cluster-info 缺少 kubeconfig")
	}
	if got := ci.Data[discovery.ClusterIDKey]; got != "cid-1234" {
		t.Fatalf("init 应发布 clusterID（不带签名），got %q", got)
	}
	for key := range ci.Data {
		if strings.HasPrefix(key, tokenapi.JWSSignatureKeyPrefix) {
			t.Errorf("init 不应写令牌 JWS: %s", key)
		}
		if strings.HasPrefix(key, discovery.ClusterIDJWSPrefix) {
			t.Errorf("init 不应写集群身份 JWS: %s", key)
		}
	}
	// kubeconfig 必须内嵌 CA 且指向可外部访问的地址（不能是 127.0.0.1）。
	blob, err := clientcmd.Load([]byte(ci.Data[tokenapi.KubeConfigKey]))
	if err != nil {
		t.Fatalf("kubeconfig 不可解析: %v", err)
	}
	cluster := blob.Clusters["kubernetes"]
	if cluster == nil || len(cluster.CertificateAuthorityData) == 0 {
		t.Fatal("kubeconfig 未内嵌 CA data")
	}
	if strings.Contains(cluster.Server, "127.0.0.1") {
		t.Fatalf("cluster-info 的 server 不能是回环地址: %s", cluster.Server)
	}
}

// TestClusterInfoUsesExternalEndpoint 优先公布 externalEndpoints。
func TestClusterInfoUsesExternalEndpoint(t *testing.T) {
	opts := testOptions()
	opts.CACertPEM = pemCA(t)
	opts.Cluster.ControlPlane.ExternalEndpoints = []string{"k8s.example.com"}
	cm, err := clusterInfoConfigMap(opts)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := clientcmd.Load([]byte(cm.Data["kubeconfig"]))
	if err != nil {
		t.Fatal(err)
	}
	if got := blob.Clusters["kubernetes"].Server; got != "https://k8s.example.com:6443" {
		t.Fatalf("server = %s", got)
	}
}

// TestClusterInfoRequiresCA: 没有 CA 就不能生成 cluster-info（必须先跑 certs）。
func TestClusterInfoRequiresCA(t *testing.T) {
	opts := testOptions()
	opts.Paths = constants.NewPaths(t.TempDir())
	if _, err := clusterInfoConfigMap(opts); err == nil {
		t.Fatal("没有 CA 时必须报错")
	}
}

func TestApplyPolicyUpdateCarriesResourceVersion(t *testing.T) {
	ctx := context.Background()
	rs, err := Build(testOptions())
	if err != nil {
		t.Fatal(err)
	}
	client := fake.NewSimpleClientset()
	// fake clientset 不会像 apiserver 那样给对象分配 resourceVersion，
	// 而生产代码正是靠 Get 拿到的 RV 去 Update 的：这里先补上 create 赋 RV 的行为，
	// 再在 update 上强制要求它（否则测不出真实约束）。
	assign := func(kind string) k8stesting.ReactionFunc {
		return func(action k8stesting.Action) (bool, runtime.Object, error) {
			obj := action.(k8stesting.CreateAction).GetObject()
			accessor, err := meta.Accessor(obj)
			if err != nil {
				t.Fatalf("取 metadata 失败: %v", err)
			}
			accessor.SetResourceVersion("1")
			return false, nil, nil
		}
	}
	client.PrependReactor("create", "validatingadmissionpolicies", assign("validatingadmissionpolicies"))
	client.PrependReactor("create", "validatingadmissionpolicybindings", assign("validatingadmissionpolicybindings"))
	client.PrependReactor("update", "validatingadmissionpolicies", checkRVOnUpdate(t, "validatingadmissionpolicies"))
	client.PrependReactor("update", "validatingadmissionpolicybindings", checkRVOnUpdate(t, "validatingadmissionpolicybindings"))

	if err := rs.Apply(ctx, client, nil); err != nil {
		t.Fatalf("首次 Apply: %v", err)
	}
	if err := rs.Apply(ctx, client, nil); err != nil {
		t.Fatalf("第二次 Apply 必须带 resourceVersion 才能成功: %v", err)
	}
}

// checkRVOnUpdate 模拟 apiserver 对 update 的 resourceVersion 校验。
func checkRVOnUpdate(t *testing.T, kind string) k8stesting.ReactionFunc {
	t.Helper()
	return func(action k8stesting.Action) (bool, runtime.Object, error) {
		obj := action.(k8stesting.UpdateAction).GetObject()
		accessor, err := meta.Accessor(obj)
		if err != nil {
			t.Fatalf("取 metadata 失败: %v", err)
		}
		if accessor.GetResourceVersion() == "" {
			return true, nil, fmt.Errorf("%s %q 的 update 缺少 resourceVersion（真实 apiserver 会拒绝）",
				kind, accessor.GetName())
		}
		return false, nil, nil
	}
}

// TestCheckDetectsDriftedObjects 锁定 cluster-init 的 Check 与 Apply 同源：
// 对象存在但内容被改过时必须返回 false，否则 node repair 会跳过收敛。
func TestCheckDetectsDriftedObjects(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	rs, err := Build(testOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := rs.Apply(ctx, client, nil); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	cm, err := client.CoreV1().ConfigMaps(constants.KubeSystemNamespace).Get(ctx, constants.ClusterStateName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("读取 cluster-state ConfigMap: %v", err)
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data["release"] = "v0.0.0-drift"
	if _, err := client.CoreV1().ConfigMaps(constants.KubeSystemNamespace).Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("制造漂移: %v", err)
	}
	done, err := rs.Check(ctx, client)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if done {
		t.Fatal("内容漂移后 Check 不应报告完成")
	}
}

// TestCheckIgnoresTokenCreateKeys: cluster-info 里由 token create 写入的 JWS/clusterID
// 不属于 cluster-init 的受管字段；它们存在时 Check 仍应报告完成，避免 node repair
// 每次都重跑 cluster-init。
func TestCheckIgnoresTokenCreateKeys(t *testing.T) {
	ctx := context.Background()
	client := fake.NewSimpleClientset()
	opts := testOptions()
	opts.CACertPEM = pemCA(t)
	rs, err := Build(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := rs.Apply(ctx, client, nil); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	cm, err := client.CoreV1().ConfigMaps(constants.KubePublicNamespace).Get(ctx, constants.ClusterInfoName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cm.Data[discovery.ClusterIDKey] = "cid-1234"
	cm.Data[discovery.ClusterIDJWSPrefix+"abcdef"] = "id-sig"
	cm.Data[tokenapi.JWSSignatureKeyPrefix+"abcdef"] = "kc-sig"
	if _, err := client.CoreV1().ConfigMaps(constants.KubePublicNamespace).Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	done, err := rs.Check(ctx, client)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !done {
		t.Fatal("token create 写入的动态键不应让 cluster-init 判定漂移")
	}

	// 受管字段（kubeconfig）被改坏时必须仍然能检测到。
	cm.Data[tokenapi.KubeConfigKey] = "tampered"
	if _, err := client.CoreV1().ConfigMaps(constants.KubePublicNamespace).Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	done, err = rs.Check(ctx, client)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if done {
		t.Fatal("cluster-info 的 kubeconfig 漂移必须被检测到")
	}
}
