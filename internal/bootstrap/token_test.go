package bootstrap

import (
	"testing"
	"time"

	tokenapi "k8s.io/cluster-bootstrap/token/api"

	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
)

// TestNewSecretGivesSigningUsageToBothRoles 覆盖真机踩到的坑。
// cluster-info 的 kubeconfig 由 kube-controller-manager 的 bootstrap-signer
// 逐令牌签名，而它只处理带 usage-bootstrap-signing 的令牌。master 令牌必须带
// 这个 usage，否则加入的 master 无法通过 JWS 校验、永远加不进 HA 集群
// （真机实测：m2/m3 探测失败，且错误被端点回退掩盖成"最后一个端点连接失败"）。
func TestNewSecretGivesSigningUsageToBothRoles(t *testing.T) {
	for _, role := range []clusterconfig.Role{clusterconfig.RoleMaster, clusterconfig.RoleWorker} {
		token, err := NewToken()
		if err != nil {
			t.Fatal(err)
		}
		secret, err := NewSecret(SecretConfig{
			Token: token, Role: role, TTL: time.Hour, Signing: true,
		})
		if err != nil {
			t.Fatalf("NewSecret(%s): %v", role, err)
		}
		if got := string(secret.Data[tokenapi.BootstrapTokenUsageSigningKey]); got != "true" {
			t.Errorf("%s 令牌必须带 usage-bootstrap-signing，否则 KCM 不会为它签 cluster-info（实际 %q）", role, got)
		}
	}
}

// TestNewSecretDefaultsToTwoHoursAndRejectsLongerTTL 固定 join 令牌的硬上限。
// 采用短期凭证模型：令牌过期后必须重新签发 join 命令；
// 任何超过 2h 的 TTL 都必须在创建 Secret 时被拒绝。
func TestNewSecretDefaultsToTwoHoursAndRejectsLongerTTL(t *testing.T) {
	tok := Token{ID: "abcdef", Secret: "0123456789abcdef"}
	secret, err := NewSecret(SecretConfig{Token: tok, Role: clusterconfig.RoleWorker})
	if err != nil {
		t.Fatalf("NewSecret: %v", err)
	}
	expiry, err := time.Parse(time.RFC3339, string(secret.Data[tokenapi.BootstrapTokenExpirationKey]))
	if err != nil {
		t.Fatalf("parse expiration: %v", err)
	}
	remaining := time.Until(expiry)
	if remaining > constants.MaxBootstrapTokenTTL+time.Minute || remaining < constants.MaxBootstrapTokenTTL-time.Minute {
		t.Fatalf("default TTL = %s, want ~%s", remaining.Round(time.Second), constants.MaxBootstrapTokenTTL)
	}
	if _, err := NewSecret(SecretConfig{
		Token: tok, Role: clusterconfig.RoleWorker, TTL: constants.MaxBootstrapTokenTTL + time.Second,
	}); err == nil {
		t.Fatal("TTL above maximum must be rejected")
	}
}
