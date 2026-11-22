package kubeconfig

import (
	"os"
	"testing"
)

// TestMain 把 HOME 指向临时目录，避免测试写入开发者真实的 ~/.kube/config。
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "kubemono-kubeconfig-home-")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("HOME", home)
	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
}
