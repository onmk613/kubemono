package discovery

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"kubemono/internal/bootstrap"
	"kubemono/internal/constants"
)

// fakeReader 按 endpoint 返回预置的 ConfigMap。
type fakeReader struct {
	byServer map[string]*corev1.ConfigMap
	errs     map[string]error
	calls    []string
}

func (f *fakeReader) ClusterInfo(_ context.Context, server string) (*corev1.ConfigMap, error) {
	f.calls = append(f.calls, server)
	if err := f.errs[server]; err != nil {
		return nil, err
	}
	if cm, ok := f.byServer[server]; ok {
		return cm, nil
	}
	return nil, errors.New("connection refused")
}

// clusterInfo 为某个令牌构造带签名的 cluster-info ConfigMap。
func clusterInfo(t *testing.T, token bootstrap.Token, caPEM []byte, clusterID string) *corev1.ConfigMap {
	t.Helper()
	kubeconfigData := bootstrapKubeconfig(t, caPEM, "https://10.0.0.1:6443")
	sig, err := bootstrap.ComputeDetachedSignature(string(kubeconfigData), token)
	if err != nil {
		t.Fatal(err)
	}
	data := map[string]string{
		"kubeconfig":                 string(kubeconfigData),
		"jws-kubeconfig-" + token.ID: sig,
	}
	if clusterID != "" {
		idSig, err := bootstrap.ComputeDetachedSignature(clusterID, token)
		if err != nil {
			t.Fatal(err)
		}
		data[ClusterIDKey] = clusterID
		data[ClusterIDJWSPrefix+token.ID] = idSig
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: constants.ClusterInfoName, Namespace: constants.KubePublicNamespace},
		Data:       data,
	}
}

func bootstrapKubeconfig(t *testing.T, caPEM []byte, server string) []byte {
	t.Helper()
	cfg := clientcmdapi.Config{
		Clusters: map[string]*clientcmdapi.Cluster{
			"kubernetes": {Server: server, CertificateAuthorityData: caPEM},
		},
		AuthInfos: map[string]*clientcmdapi.AuthInfo{
			"kubelet-bootstrap": {Token: "aaaaaaaa.bbbbbbbbbbbbbbbb"},
		},
		Contexts: map[string]*clientcmdapi.Context{
			"kubernetes": {Cluster: "kubernetes", AuthInfo: "kubelet-bootstrap"},
		},
		CurrentContext: "kubernetes",
	}
	data, err := clientcmd.Write(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func testCA(t *testing.T) []byte {
	t.Helper()
	// 用 internal/bootstrap 的测试同样的办法生成一张自签 CA：这里只需要 DER→PEM。
	return pemCert(t)
}

func TestProbeVerifiesSignatureAndIdentity(t *testing.T) {
	token, err := bootstrap.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	ca := testCA(t)
	reader := &fakeReader{byServer: map[string]*corev1.ConfigMap{
		"https://10.0.0.1:6443": clusterInfo(t, token, ca, "cid-1"),
	}}
	info, err := Probe(context.Background(), Options{
		Endpoints:         []string{"10.0.0.1"},
		Token:             token,
		Reader:            reader,
		ExpectedClusterID: "cid-1",
	})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if info == nil {
		t.Fatal("应探测到集群")
	}
	if info.ClusterID != "cid-1" {
		t.Fatalf("探测结果不正确: %+v", info)
	}
	if len(info.CA) == 0 {
		t.Fatal("CA 应为空")
	}
}

func TestProbeSkipsUntrustedResponses(t *testing.T) {
	token, _ := bootstrap.NewToken()
	other, _ := bootstrap.NewToken()
	ca := testCA(t)

	// 第一台返回的 cluster-info 是用别的令牌签的 → 不可信；第二台合法。
	reader := &fakeReader{byServer: map[string]*corev1.ConfigMap{
		"https://10.0.0.1:6443": clusterInfo(t, other, ca, "cid-1"),
		"https://10.0.0.2:6443": clusterInfo(t, token, ca, "cid-1"),
	}}
	info, err := Probe(context.Background(), Options{
		Endpoints: []string{"10.0.0.1", "10.0.0.2"},
		Token:     token,
		Reader:    reader,
	})
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if info == nil || info.Endpoint != "https://10.0.0.2:6443" {
		t.Fatalf("应回退到可信的 master: %+v", info)
	}
	if len(reader.calls) != 2 {
		t.Fatalf("应尝试两个 endpoint: %v", reader.calls)
	}
}

func TestProbeRejectsUnsignedClusterIdentity(t *testing.T) {
	token, _ := bootstrap.NewToken()
	ca := testCA(t)
	cm := clusterInfo(t, token, ca, "")
	// 有 clusterID 但没有签名：必须拒绝。
	cm.Data[ClusterIDKey] = "cid-1"
	reader := &fakeReader{byServer: map[string]*corev1.ConfigMap{"https://10.0.0.1:6443": cm}}
	if _, err := Probe(context.Background(), Options{
		Endpoints: []string{"10.0.0.1"}, Token: token, Reader: reader,
	}); err == nil {
		t.Fatal("未签名的集群身份必须被拒绝")
	}
}

func TestProbeRejectsWrongClusterID(t *testing.T) {
	token, _ := bootstrap.NewToken()
	ca := testCA(t)
	reader := &fakeReader{byServer: map[string]*corev1.ConfigMap{
		"https://10.0.0.1:6443": clusterInfo(t, token, ca, "cid-1"),
	}}
	if _, err := Probe(context.Background(), Options{
		Endpoints: []string{"10.0.0.1"}, Token: token, Reader: reader,
		ExpectedClusterID: "other",
	}); err == nil {
		t.Fatal("clusterID 不一致必须拒绝")
	}
}

func TestProbeHonoursCACertPins(t *testing.T) {
	token, _ := bootstrap.NewToken()
	ca := testCA(t)
	reader := &fakeReader{byServer: map[string]*corev1.ConfigMap{
		"https://10.0.0.1:6443": clusterInfo(t, token, ca, "cid-1"),
	}}
	if _, err := Probe(context.Background(), Options{
		Endpoints: []string{"10.0.0.1"}, Token: token, Reader: reader,
		CACertHashes: []string{"sha256:0000000000000000000000000000000000000000000000000000000000000000"},
	}); err == nil {
		t.Fatal("CA pin 不匹配必须拒绝")
	}
	// 正确的 pin 可以通过。
	hashes, err := bootstrap.CACertHashes(ca)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Probe(context.Background(), Options{
		Endpoints: []string{"10.0.0.1"}, Token: token, Reader: reader, CACertHashes: hashes,
	}); err != nil {
		t.Fatalf("正确的 pin 应通过: %v", err)
	}
}

func TestProbeWithoutTokenFails(t *testing.T) {
	if _, err := Probe(context.Background(), Options{Endpoints: []string{"10.0.0.1"}}); err == nil {
		t.Fatal("没有引导令牌无法校验签名，必须报错")
	}
}

func TestProbeReturnsErrorWhenNoEndpointAnswers(t *testing.T) {
	token, _ := bootstrap.NewToken()
	reader := &fakeReader{}
	_, err := Probe(context.Background(), Options{
		Endpoints: []string{"10.0.0.1", "10.0.0.2"}, Token: token, Reader: reader,
	})
	if err == nil {
		t.Fatal("全部 endpoint 失败时应返回错误（区别于“没有集群”）")
	}
}

func TestEndpointURL(t *testing.T) {
	cases := map[string]string{
		"10.0.0.1":              "https://10.0.0.1:6443",
		"10.0.0.1:7443":         "https://10.0.0.1:7443",
		"https://k8s.local":     "https://k8s.local",
		"https://k8s.local:443": "https://k8s.local:443",
	}
	for in, want := range cases {
		if got := endpointURL(in, 6443); got != want {
			t.Errorf("endpointURL(%q) = %q, want %q", in, got, want)
		}
	}
}

var _ = time.Second

func TestUnreachableEndpointsAreRetryableButVerificationErrorsAreNot(t *testing.T) {
	token, _ := bootstrap.NewToken()
	unreachable := &fakeReader{errs: map[string]error{
		"https://10.0.0.1:6443": errors.New("dial tcp 10.0.0.1:6443: connect: connection refused"),
	}}
	_, err := Probe(context.Background(), Options{
		Endpoints: []string{"10.0.0.1"}, Token: token, Reader: unreachable,
	})
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("Probe error = %v, want ErrUnreachable", err)
	}
	// 签名/身份失败不能被当成“集群还没起来”而无限重试。
	ca := testCA(t)
	other, _ := bootstrap.NewToken()
	verifyFailure := &fakeReader{byServer: map[string]*corev1.ConfigMap{
		"https://10.0.0.1:6443": clusterInfo(t, other, ca, "cid-1"),
	}}
	_, err = Probe(context.Background(), Options{
		Endpoints: []string{"10.0.0.1"}, Token: token, Reader: verifyFailure,
	})
	if err == nil || errors.Is(err, ErrUnreachable) {
		t.Fatalf("verification error = %v，不能返回 ErrUnreachable", err)
	}
}
