package pki

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"strings"
	"testing"
	"time"

	clusterconfig "kubemono/internal/config"
)

// TestKeyAlgorithmMirrorsV1 锁定 pki 与 v1 的算法字符串一致，
// 这样 cluster.yaml 的 controlPlane.pki.keyAlgorithm 可以直接交给本包。
func TestKeyAlgorithmMirrorsV1(t *testing.T) {
	if got, want := string(RSA2048), clusterconfig.KeyAlgorithmRSA2048; got != want {
		t.Errorf("RSA2048 = %q, clusterconfig.KeyAlgorithmRSA2048 = %q", got, want)
	}
	if got, want := string(ECDSAP256), clusterconfig.KeyAlgorithmECDSAP256; got != want {
		t.Errorf("ECDSAP256 = %q, clusterconfig.KeyAlgorithmECDSAP256 = %q", got, want)
	}
	if DefaultKeyAlgorithm != RSA2048 {
		t.Errorf("DefaultKeyAlgorithm = %q, 期望 %q", DefaultKeyAlgorithm, RSA2048)
	}
}

func TestKeyAlgorithmString(t *testing.T) {
	tests := []struct {
		name string
		alg  KeyAlgorithm
		want string
	}{
		{name: "rsa", alg: RSA2048, want: "rsa2048"},
		{name: "ecdsa", alg: ECDSAP256, want: "ecdsa-p256"},
		{name: "empty", alg: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.alg.String(); got != tt.want {
				t.Fatalf("String() = %q, 期望 %q", got, tt.want)
			}
		})
	}
}

func TestKeyAlgorithmValid(t *testing.T) {
	tests := []struct {
		name string
		alg  KeyAlgorithm
		want bool
	}{
		{name: "rsa2048", alg: RSA2048, want: true},
		{name: "ecdsa-p256", alg: ECDSAP256, want: true},
		{name: "empty", alg: "", want: false},
		{name: "unknown", alg: "ed25519", want: false},
		{name: "rsa4096", alg: "rsa4096", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.alg.Valid(); got != tt.want {
				t.Fatalf("Valid() = %v, 期望 %v", got, tt.want)
			}
		})
	}
}

func TestKeyAlgorithmNewPrivateKey(t *testing.T) {
	tests := []struct {
		name     string
		alg      KeyAlgorithm
		wantType any
		wantErr  bool
	}{
		{name: "rsa2048", alg: RSA2048, wantType: &rsa.PrivateKey{}},
		{name: "ecdsa-p256", alg: ECDSAP256, wantType: &ecdsa.PrivateKey{}},
		{name: "空算法视为默认", alg: "", wantErr: true},
		{name: "未知算法", alg: KeyAlgorithm("ed25519"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			signer, err := tt.alg.NewPrivateKey()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("期望错误，得到 %T", signer)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewPrivateKey() 失败: %v", err)
			}
			switch want := tt.wantType.(type) {
			case *rsa.PrivateKey:
				key, ok := signer.(*rsa.PrivateKey)
				if !ok {
					t.Fatalf("类型 = %T, 期望 %T", signer, want)
				}
				if bits := key.N.BitLen(); bits != 2048 {
					t.Fatalf("RSA 位数 = %d, 期望 2048", bits)
				}
			case *ecdsa.PrivateKey:
				key, ok := signer.(*ecdsa.PrivateKey)
				if !ok {
					t.Fatalf("类型 = %T, 期望 %T", signer, want)
				}
				if key.Curve.Params().BitSize != 256 {
					t.Fatalf("曲线位数 = %d, 期望 256", key.Curve.Params().BitSize)
				}
			}
		})
	}
}

func TestKeyAlgorithmOf(t *testing.T) {
	rsaKey, err := RSA2048.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	ecKey, err := ECDSAP256.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	_, edKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		key     crypto.Signer
		want    KeyAlgorithm
		wantErr bool
	}{
		{name: "rsa", key: rsaKey, want: RSA2048},
		{name: "ecdsa", key: ecKey, want: ECDSAP256},
		{name: "ed25519", key: edKey, wantErr: true},
		{name: "nil", key: nil, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := KeyAlgorithmOf(tt.key)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("期望错误，得到 %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("KeyAlgorithmOf() 失败: %v", err)
			}
			if got != tt.want {
				t.Fatalf("KeyAlgorithmOf() = %q, 期望 %q", got, tt.want)
			}
		})
	}
}

func TestEncodeKeyPEMAndParsePrivateKeyPEM(t *testing.T) {
	rsaKey, err := RSA2048.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	ecKey, err := ECDSAP256.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	rsaPEM, err := EncodeKeyPEM(rsaKey)
	if err != nil {
		t.Fatal(err)
	}
	ecPEM, err := EncodeKeyPEM(ecKey)
	if err != nil {
		t.Fatal(err)
	}
	if block, _ := pem.Decode(rsaPEM); block == nil || block.Type != "PRIVATE KEY" {
		t.Fatalf("RSA 私钥 PEM 类型应为 PKCS#8 PRIVATE KEY，实际 %+v", block)
	}
	pkcs1PEM := pemBlock("RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(rsaKey.(*rsa.PrivateKey)))
	sec1DER, err := x509.MarshalECPrivateKey(ecKey.(*ecdsa.PrivateKey))
	if err != nil {
		t.Fatal(err)
	}
	sec1PEM := pemBlock("EC PRIVATE KEY", sec1DER)
	cert, err := x509.ParseCertificate(selfSigned(t, ecKey, testCertTemplate("mixed", time.Now().Add(-time.Hour), time.Now().Add(time.Hour)), nil, nil))
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		in      []byte
		wantPub crypto.PublicKey
		wantErr bool
	}{
		{name: "pkcs8 rsa", in: rsaPEM, wantPub: rsaKey.Public()},
		{name: "pkcs8 ecdsa", in: ecPEM, wantPub: ecKey.Public()},
		{name: "pkcs1 rsa", in: pkcs1PEM, wantPub: rsaKey.Public()},
		{name: "sec1 ecdsa", in: sec1PEM, wantPub: ecKey.Public()},
		{
			name:    "证书块在前也能找到私钥",
			in:      append(EncodeCertPEM(cert), ecPEM...),
			wantPub: ecKey.Public(),
		},
		{name: "空输入", in: nil, wantErr: true},
		{name: "只有证书", in: EncodeCertPEM(cert), wantErr: true},
		{name: "被篡改的私钥", in: tamper(rsaPEM), wantErr: true},
		{name: "非 PEM", in: []byte("not a pem"), wantErr: true},
		{name: "加密私钥不支持", in: pemBlock("ENCRYPTED PRIVATE KEY", []byte("x")), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			signer, err := ParsePrivateKeyPEM(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("期望错误，得到 %T", signer)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParsePrivateKeyPEM() 失败: %v", err)
			}
			if !publicKeysEqual(signer.Public(), tt.wantPub) {
				t.Fatalf("解析出的公钥与期望不一致")
			}
		})
	}

	if _, err := EncodeKeyPEM(nil); err == nil {
		t.Fatal("EncodeKeyPEM(nil) 期望错误")
	}
}

func TestEncodeCertPEMAndParseCertsPEM(t *testing.T) {
	ca := cloneCA(t, ECDSAP256)
	leaf, _, err := ca.Sign(CertConfig{CommonName: "leaf", Validity: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	leafPEM := EncodeCertPEM(leaf)
	caPEM := EncodeCertPEM(ca.Cert)

	tests := []struct {
		name    string
		in      []byte
		wantLen int
		wantErr bool
	}{
		{name: "单证书", in: leafPEM, wantLen: 1},
		{name: "多证书", in: append(append([]byte(nil), leafPEM...), caPEM...), wantLen: 2},
		{name: "空输入", in: nil, wantErr: true},
		{name: "被篡改", in: tamper(leafPEM), wantErr: true},
		{name: "只有私钥块", in: mustKeyPEM(t), wantErr: true},
		{name: "非 PEM", in: []byte("garbage"), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			certs, err := ParseCertsPEM(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("期望错误，得到 %d 张证书", len(certs))
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseCertsPEM() 失败: %v", err)
			}
			if len(certs) != tt.wantLen {
				t.Fatalf("证书数量 = %d, 期望 %d", len(certs), tt.wantLen)
			}
			for _, cert := range certs {
				if cert.Subject.CommonName == "" {
					t.Fatalf("解析出的证书缺少 CN")
				}
			}
		})
	}

	if got := EncodeCertPEM(nil); got != nil {
		t.Fatalf("EncodeCertPEM(nil) = %q, 期望 nil", got)
	}
	if got := EncodeCertPEM(&x509.Certificate{}); got != nil {
		t.Fatalf("EncodeCertPEM(无 DER) = %q, 期望 nil", got)
	}
}

func mustKeyPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ECDSAP256.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	data, err := EncodeKeyPEM(key)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestEncodeparsePublicKeyPEM(t *testing.T) {
	rsaKey, err := RSA2048.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	ecKey, err := ECDSAP256.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	rsaPub, err := encodePublicKeyPEM(rsaKey.Public())
	if err != nil {
		t.Fatal(err)
	}
	ecPub, err := encodePublicKeyPEM(ecKey.Public())
	if err != nil {
		t.Fatal(err)
	}
	pkcs1Pub := pemBlock("RSA PUBLIC KEY", x509.MarshalPKCS1PublicKey(rsaKey.Public().(*rsa.PublicKey)))
	tests := []struct {
		name    string
		in      []byte
		want    crypto.PublicKey
		wantErr bool
	}{
		{name: "pkix rsa", in: rsaPub, want: rsaKey.Public()},
		{name: "pkix ecdsa", in: ecPub, want: ecKey.Public()},
		{name: "pkcs1 rsa", in: pkcs1Pub, want: rsaKey.Public()},
		{name: "空", in: nil, wantErr: true},
		{name: "被篡改", in: tamper(rsaPub), wantErr: true},
		{name: "私钥块", in: mustKeyPEM(t), wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pub, err := parsePublicKeyPEM(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("期望错误，得到 %T", pub)
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePublicKeyPEM() 失败: %v", err)
			}
			if !publicKeysEqual(pub, tt.want) {
				t.Fatalf("公钥不一致")
			}
		})
	}
	if _, err := encodePublicKeyPEM(nil); err == nil {
		t.Fatal("encodePublicKeyPEM(nil) 期望错误")
	}
}

func TestErrorsDoNotLeakKeyMaterial(t *testing.T) {
	key, err := ECDSAP256.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, err := EncodeKeyPEM(key)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		t.Fatal("私钥 PEM 解析失败")
	}
	needle := base64.StdEncoding.EncodeToString(block.Bytes[:12])

	_, errParse := ParsePrivateKeyPEM(tamper(keyPEM))
	if errParse == nil {
		t.Fatal("期望解析被篡改的私钥失败")
	}
	for name, err := range map[string]error{"tampered": errParse} {
		if strings.Contains(err.Error(), needle) {
			t.Fatalf("%s 错误信息泄露了私钥材料: %v", name, err)
		}
		if strings.Contains(err.Error(), "BEGIN") {
			t.Fatalf("%s 错误信息包含 PEM 内容: %v", name, err)
		}
	}
}
