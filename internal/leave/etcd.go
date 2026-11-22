package leave

import (
	"context"
	"fmt"
	"strings"
	"time"

	clusterconfig "kubemono/internal/config"
	"kubemono/internal/constants"
	etcdapi "kubemono/internal/etcd"
)

// EtcdMembers 抽象 etcd 成员管理（可注入：单测用 fake 验证法定人数校验）。
// 成员类型直接复用 internal/etcd.Member，避免自造同构类型后在 List 里
// 逐字段搬运，两套类型漂移时很难被发现。
type EtcdMembers interface {
	// List 返回全部成员及其健康状态。
	List(ctx context.Context) ([]etcdapi.Member, error)
	// CanRemove 校验移除指定成员是否安全；不满足法定人数时返回错误。
	CanRemove(ctx context.Context, id uint64) error
	// Remove 移除一个成员。
	Remove(ctx context.Context, id uint64) error
}

// MembersClient 是基于 internal/etcd 的生产实现。
// 法定人数校验、成员移除与 TLS 连接都复用 internal/etcd，避免运维入口
// 和 phase 各自维护一套判断。
type MembersClient struct {
	// Endpoints 是 etcd 客户端地址（https://host:2379）。
	Endpoints []string
	// CACert、Cert、Key 是 mTLS 材料；三者都为空时不启用 TLS。
	CACert string
	Cert   string
	Key    string
	// Timeout 是单次操作超时（默认 5 秒）。
	Timeout time.Duration
}

// NewMembersClient 返回 etcd 成员客户端。
func NewMembersClient(endpoints []string, ca, cert, key string) *MembersClient {
	return &MembersClient{Endpoints: endpoints, CACert: ca, Cert: cert, Key: key}
}

func (c *MembersClient) timeout() time.Duration {
	if c.Timeout <= 0 {
		return 5 * time.Second
	}
	return c.Timeout
}

// client 建立一个短连接客户端（每次调用后关闭，避免长期持有连接）。
func (c *MembersClient) client() (*etcdapi.Client, error) {
	if len(c.Endpoints) == 0 {
		return nil, fmt.Errorf("没有配置 etcd 端点，无法管理 etcd 成员")
	}
	cli, err := etcdapi.New(etcdapi.Config{
		Endpoints:      c.Endpoints,
		TLS:            etcdapi.TLSFiles{CAFile: c.CACert, CertFile: c.Cert, KeyFile: c.Key},
		DialTimeout:    c.timeout(),
		RequestTimeout: c.timeout(),
	})
	if err != nil {
		return nil, fmt.Errorf("连接 etcd（%s）失败: %w", strings.Join(c.Endpoints, ","), err)
	}
	return cli, nil
}

// List 实现 EtcdMembers：直接返回 internal/etcd 的成员列表。
func (c *MembersClient) List(ctx context.Context) ([]etcdapi.Member, error) {
	cli, err := c.client()
	if err != nil {
		return nil, err
	}
	defer func() { _ = cli.Close() }()
	return cli.Members(ctx)
}

// CanRemove 实现 EtcdMembers：复用 internal/etcd 的法定人数策略。
func (c *MembersClient) CanRemove(ctx context.Context, id uint64) error {
	cli, err := c.client()
	if err != nil {
		return err
	}
	defer func() { _ = cli.Close() }()
	return cli.CanRemoveMember(ctx, id)
}

// Remove 实现 EtcdMembers：复用 internal/etcd 的幂等移除语义。
func (c *MembersClient) Remove(ctx context.Context, id uint64) error {
	cli, err := c.client()
	if err != nil {
		return err
	}
	defer func() { _ = cli.Close() }()
	return cli.RemoveMember(ctx, id)
}

// EtcdEndpointsAndTLS 从集群配置推导 etcd 客户端的端点与 mTLS 材料。
// - stacked：所有 master 的 https://<address>:2379；证书用 kubemono 的 etcd CA 与
// apiserver-etcd-client（与 apiserver 使用同一份凭据）；
// - external：用户提供的 endpoints；
// - external 接入已有集群：使用配置里声明的 endpoints 与 CA/证书/私钥文件。
func EtcdEndpointsAndTLS(paths constants.Paths, cluster *clusterconfig.Cluster) (endpoints []string, ca, cert, key string) {
	ca = paths.EtcdCertPath(constants.CACertName)
	cert = paths.CertPath(constants.APIServerEtcdClientCertName)
	key = paths.CertPath(constants.APIServerEtcdClientKeyName)
	if cluster == nil {
		return nil, ca, cert, key
	}
	if cluster.ControlPlane.Etcd.IsExternal() {
		external := cluster.ControlPlane.Etcd.External
		endpoints = append(endpoints, external.Endpoints...)
		if external.CAFile != "" {
			ca = external.CAFile
		}
		if external.CertFile != "" {
			cert = external.CertFile
		}
		if external.KeyFile != "" {
			key = external.KeyFile
		}
	} else {
		for _, host := range cluster.Cluster.Hosts.Masters() {
			endpoints = append(endpoints, fmt.Sprintf("https://%s:%d", host.Address, constants.EtcdClientPort))
		}
	}
	return endpoints, ca, cert, key
}
