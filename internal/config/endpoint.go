package config

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// EndpointHost 从 host[:port] 或 URL 中提取主机名；解析失败时返回空串。
func EndpointHost(endpoint string) string {
	raw := strings.TrimSpace(endpoint)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// NormalizeEndpoint 把 apiserver 地址规范成 https://host:port 形式。
// 缺少端口时补 defaultPort；空串或无法解析时返回 ok=false。
// 这是 endpoint 渲染的唯一实现：kubeconfig、network 与 cluster-info
// 都必须落成同一种 URL 形式。
func NormalizeEndpoint(endpoint string, defaultPort int) (string, bool) {
	ep := strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if ep == "" {
		return "", false
	}
	ep = strings.TrimPrefix(ep, "https://")
	ep = strings.TrimPrefix(ep, "http://")
	if ep == "" || strings.HasSuffix(ep, ":") {
		return "", false
	}
	host, port, err := net.SplitHostPort(ep)
	if err != nil {
		host = strings.Trim(ep, "[]")
		port = strconv.Itoa(defaultPort)
	}
	if n, err := strconv.Atoi(port); err != nil || n <= 0 {
		port = strconv.Itoa(defaultPort)
	}
	if host == "" {
		return "", false
	}
	return "https://" + net.JoinHostPort(host, port), true
}

// ExternalEndpointsAPIEndpoint 返回 externalEndpoints[0]。
// TLS Bootstrap、worker 直连与 cluster-info 都用它：这里要求一个节点可直连的
// 稳定入口（VIP/DNS）。
func (c *Cluster) ExternalEndpointsAPIEndpoint(defaultPort int) (string, bool) {
	if c == nil || len(c.ControlPlane.ExternalEndpoints) == 0 {
		return "", false
	}
	return NormalizeEndpoint(c.ControlPlane.ExternalEndpoints[0], defaultPort)
}

// FirstMasterAPIEndpoint 返回 cluster.hosts 中第一个有地址的 master。
func (c *Cluster) FirstMasterAPIEndpoint(defaultPort int) (string, bool) {
	if c == nil {
		return "", false
	}
	for _, m := range c.Cluster.Hosts.Masters() {
		if m.Address == "" {
			continue
		}
		return NormalizeEndpoint(net.JoinHostPort(m.Address, strconv.Itoa(defaultPort)), defaultPort)
	}
	return "", false
}

// ParseAPIEndpoint 严格解析一个 apiserver 入口（host[:port] 或 https://host[:port]）。
// 与 NormalizeEndpoint 的区别：后者为渲染路径做“尽量能解析”的兜底，
// 这里用于配置校验，必须拒绝路径、查询串、非法端口和 IPv6。
func ParseAPIEndpoint(endpoint string, defaultPort int) (string, error) {
	raw := strings.TrimSpace(endpoint)
	if raw == "" {
		return "", fmt.Errorf("endpoint must not be empty")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid endpoint: %w", err)
	}
	if u.Scheme != "https" {
		return "", fmt.Errorf("endpoint scheme must be https, got %q", u.Scheme)
	}
	if u.User != nil {
		return "", fmt.Errorf("endpoint must not contain userinfo")
	}
	if u.Path != "" && u.Path != "/" {
		return "", fmt.Errorf("endpoint must not contain a path")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("endpoint must not contain query or fragment")
	}
	host := u.Hostname()
	if host == "" {
		return "", fmt.Errorf("endpoint has no host")
	}
	if strings.Contains(host, ":") {
		return "", fmt.Errorf("IPv6 endpoints are not supported yet")
	}
	port := u.Port()
	if port == "" {
		port = strconv.Itoa(defaultPort)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n <= 0 || n > 65535 {
		return "", fmt.Errorf("invalid port %q", port)
	}
	return "https://" + net.JoinHostPort(host, strconv.Itoa(n)), nil
}
