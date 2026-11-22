package network

import (
	"net"
	"strconv"

	"kubemono/internal/constants"
	"kubemono/internal/render"
)

// daemonSetAPIServer 返回 hostNetwork DaemonSet kubeconfig 使用的 apiserver 地址。
// master 直连本机 apiserver；worker 优先走 local-proxy，其次 externalEndpoints，
// 最后回退到第一台 master。
func daemonSetAPIServer(c *render.Context) string {
	if c == nil || c.Cluster == nil {
		return endpointURL("127.0.0.1", constants.APIServerPort)
	}
	if c.Cluster.ControlPlane.LocalProxyEnabled() {
		return endpointURL("127.0.0.1", constants.DefaultLocalProxyPort)
	}
	if ep, ok := c.Cluster.ExternalEndpointsAPIEndpoint(constants.APIServerPort); ok {
		return ep
	}
	if ep, ok := c.Cluster.FirstMasterAPIEndpoint(constants.APIServerPort); ok {
		return ep
	}
	return endpointURL("127.0.0.1", constants.APIServerPort)
}

func endpointURL(host string, port int) string {
	return "https://" + net.JoinHostPort(host, strconv.Itoa(port))
}
