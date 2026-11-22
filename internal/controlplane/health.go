package controlplane

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"time"
)

// HTTPChecker 通过 HTTP(S) 探测组件。
type HTTPChecker struct {
	// Client 可注入，便于测试。
	Client *http.Client
}

// NewHTTPChecker 返回一个不校验 TLS 证书的检查器。
// 探针只确认"本机组件是否已经把端口服务起来"，不承担身份校验：
// 校验由组件自己的认证/授权完成。
func NewHTTPChecker() *HTTPChecker {
	return &HTTPChecker{
		Client: &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
				DisableKeepAlives: true,
			},
		},
	}
}

// Check 实现 HealthChecker。
func (c *HTTPChecker) Check(ctx context.Context, h HealthCheck) error {
	client := c.Client
	if client == nil {
		client = NewHTTPChecker().Client
	}
	timeout := h.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, h.URL(), nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("探测 %s 失败: %w", h.URL(), err)
	}
	defer func() { _ = resp.Body.Close() }()
	if !h.Accepts(resp.StatusCode) {
		return fmt.Errorf("%s 返回 HTTP %d", h.URL(), resp.StatusCode)
	}
	return nil
}
