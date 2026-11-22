// Package bootstrap 实现自建的 bootstrap 机制：bootstrap token、
// TLS bootstrap 与 CSR 审批。
// 刻意复用 k8s.io/cluster-bootstrap 中稳定且体积很小的部分：
// 令牌格式、令牌 Secret 的字段约定、以及 cluster-info 的 JWS 验签。
// 这三处“自己实现反而更容易和上游不一致”，其余逻辑（分组、审批、校验）都是
// kubemono 自己的策略，写在这里。
package bootstrap

import (
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	tokenapi "k8s.io/cluster-bootstrap/token/api"
	tokenutil "k8s.io/cluster-bootstrap/token/util"
	"k8s.io/cluster-bootstrap/util/tokens"

	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
)

// Token 是解析后的 bootstrap token，形式为 "<id>.<secret>"。
type Token struct {
	ID     string
	Secret string
}

// String 返回传输形式 "<id>.<secret>"。
func (t Token) String() string { return tokenutil.TokenFromIDAndSecret(t.ID, t.Secret) }

// NewToken 生成一个新的、密码学随机的 bootstrap token。
func NewToken() (Token, error) {
	raw, err := tokenutil.GenerateBootstrapToken()
	if err != nil {
		return Token{}, fmt.Errorf("generate bootstrap token: %w", err)
	}
	id, secret, err := tokens.ParseToken(raw)
	if err != nil {
		return Token{}, fmt.Errorf("parse generated bootstrap token: %w", err)
	}
	return Token{ID: id, Secret: secret}, nil
}

// ParseToken 解析并校验 bootstrap token 字符串。
func ParseToken(raw string) (Token, error) {
	if !tokenutil.IsValidBootstrapToken(raw) {
		return Token{}, fmt.Errorf("invalid bootstrap token: must match [a-z0-9]{6}.[a-z0-9]{16}")
	}
	id, secret, err := tokens.ParseToken(raw)
	if err != nil {
		return Token{}, err
	}
	return Token{ID: id, Secret: secret}, nil
}

// SecretName 返回令牌 ID 对应的 kube-system Secret 名字。
func SecretName(id string) string { return tokenutil.BootstrapTokenSecretName(id) }

// SecretConfig 描述要创建的 bootstrap token Secret。
type SecretConfig struct {
	Token       Token
	Role        clusterconfig.Role
	Description string
	// TTL 是令牌有效期；<=0 时用默认值，超过 MaxBootstrapTokenTTL 会被拒绝。
	TTL time.Duration
	// Signing 允许该令牌为 cluster-info 签名。
	Signing bool
}

// NewSecret 构建 bootstrap token 对应的 kube-system Secret。
func NewSecret(cfg SecretConfig) (*corev1.Secret, error) {
	var group string
	switch cfg.Role {
	case clusterconfig.RoleWorker:
		group = constants.GroupBootstrappersWorker
	case clusterconfig.RoleMaster:
		group = constants.GroupBootstrappersMaster
	default:
		return nil, fmt.Errorf("unknown role %q", cfg.Role)
	}
	if cfg.TTL <= 0 {
		cfg.TTL = constants.DefaultBootstrapTokenTTL
	}
	if cfg.TTL > constants.MaxBootstrapTokenTTL {
		return nil, fmt.Errorf("bootstrap token TTL %s exceeds maximum %s",
			cfg.TTL, constants.MaxBootstrapTokenTTL)
	}
	data := map[string][]byte{
		tokenapi.BootstrapTokenIDKey:               []byte(cfg.Token.ID),
		tokenapi.BootstrapTokenSecretKey:           []byte(cfg.Token.Secret),
		tokenapi.BootstrapTokenUsageAuthentication: []byte("true"),
		tokenapi.BootstrapTokenExtraGroupsKey:      []byte(group),
	}
	if cfg.Signing {
		data[tokenapi.BootstrapTokenUsageSigningKey] = []byte("true")
	}
	if cfg.Description != "" {
		data[tokenapi.BootstrapTokenDescriptionKey] = []byte(cfg.Description)
	}
	data[tokenapi.BootstrapTokenExpirationKey] = []byte(time.Now().UTC().Add(cfg.TTL).Format(time.RFC3339))
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      SecretName(cfg.Token.ID),
			Namespace: constants.KubeSystemNamespace,
		},
		Type: tokenapi.SecretTypeBootstrapToken,
		Data: data,
	}, nil
}
