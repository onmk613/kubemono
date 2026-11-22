package config

import (
	"strings"
	"testing"
)

// TestValidateClusterDocumentAggregates 确认 validate 入口严格解析且不短路。
func TestValidateClusterDocumentAggregates(t *testing.T) {
	data := []byte("cluster:\n")
	_, res, err := ValidateClusterDocument(data)
	if err != nil {
		t.Fatalf("ValidateClusterDocument: %v", err)
	}
	if res.OK() {
		t.Fatal("缺少 host 的文档应校验失败")
	}
	if !strings.Contains(res.Errors.ToAggregate().Error(), "hosts") {
		t.Fatalf("校验错误应指出 hosts：%v", res.Errors)
	}
}

// TestValidateClusterDocumentAcceptsValid 确保正常文档不被额外信封检查误伤。
func TestValidateClusterDocumentAcceptsValid(t *testing.T) {
	data := []byte("cluster:\n  hosts:\n    - {name: m1, address: 10.0.0.1, roles: [master]}\n")
	_, res, err := ValidateClusterDocument(data)
	if err != nil {
		t.Fatalf("ValidateClusterDocument: %v", err)
	}
	if !res.OK() {
		t.Fatalf("合法文档不应报错：%v", res.Errors)
	}
}
