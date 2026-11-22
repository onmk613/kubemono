package config

import (
	"strings"

	"kubemono/internal/constants"
)

// KubeletRootDir 返回 kubelet 数据目录。
func (c *Cluster) KubeletRootDir() string {
	if c != nil {
		if dir := strings.TrimSpace(c.Node.Kubelet.RootDir); dir != "" {
			return dir
		}
	}
	return constants.KubeletDir
}

// EtcdDataDir 返回 etcd 数据目录。
func (c *Cluster) EtcdDataDir() string {
	if c != nil {
		if dir := strings.TrimSpace(c.ControlPlane.Etcd.Local.DataDir); dir != "" {
			return dir
		}
	}
	return constants.EtcdDataDir
}

// WithDataDirs 把集群数据目录写入 Paths。
func (c *Cluster) WithDataDirs(paths constants.Paths) constants.Paths {
	if c == nil {
		return paths
	}
	return paths.WithDataDirs(c.KubeletRootDir(), c.EtcdDataDir())
}
