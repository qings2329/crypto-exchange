package adminapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// UpstreamClient 是管理后台聚合上游微服务时使用的 HTTP 客户端。
// 上游服务（futures/user/notification/...）与 admin 共用 auth.secret 且 middleware.Auth
// 仅校验签名+过期、不强制 role，因此 admin 用自身签发的 role=admin token 即可调用任何上游端点。
// 上游统一以 response.JSON 返回 {code,data,message} 信封，Get 会自动解包 data 层。
type UpstreamClient struct {
	http  *http.Client
	token string
}

// NewUpstreamClient 构造客户端。token 为 admin 自签的 Bearer token。
func NewUpstreamClient(token string) *UpstreamClient {
	return &UpstreamClient{
		http:  &http.Client{Timeout: 3 * time.Second},
		token: token,
	}
}

// envelope 是上游统一的响应信封。
type envelope struct {
	Code    int             `json:"code"`
	Data    json.RawMessage `json:"data"`
	Message string          `json:"message"`
}

// Get 调用上游 GET 并解包 data 到 out（out 为 nil 时忽略 data）。
// 返回的错误表示调用失败或上游业务错误（code!=0），调用方据此降级。
func (c *UpstreamClient) Get(ctx context.Context, baseURL, path string, out interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("upstream %s%s -> HTTP %d", baseURL, path, resp.StatusCode)
	}
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("decode envelope: %w", err)
	}
	if env.Code != 0 {
		return fmt.Errorf("upstream %s%s -> code %d: %s", baseURL, path, env.Code, env.Message)
	}
	if out != nil && len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return fmt.Errorf("decode data: %w", err)
		}
	}
	return nil
}

// Probe 对上游做一次免鉴权健康检查（GET path），返回 HTTP 状态码与耗时。
// 用于运营看板的服务健康探测；连接失败返回 0 状态码。
func (c *UpstreamClient) Probe(ctx context.Context, baseURL, path string) (status int, elapsed time.Duration, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+path, nil)
	if err != nil {
		return 0, 0, err
	}
	start := time.Now()
	resp, err := c.http.Do(req)
	elapsed = time.Since(start)
	if err != nil {
		return 0, elapsed, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, elapsed, nil
}

// do 执行带 JSON body 的上游请求（method 可为 POST/PUT/DELETE），并解包 data 到 out。
func (c *UpstreamClient) do(ctx context.Context, method, baseURL, path string, body, out interface{}) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, baseURL+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("upstream %s%s -> HTTP %d", baseURL, path, resp.StatusCode)
	}
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return fmt.Errorf("decode envelope: %w", err)
	}
	if env.Code != 0 {
		return fmt.Errorf("upstream %s%s -> code %d: %s", baseURL, path, env.Code, env.Message)
	}
	if out != nil && len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return fmt.Errorf("decode data: %w", err)
		}
	}
	return nil
}

// Post 调用上游 POST 并解包 data 到 out（out 可为 nil）。
func (c *UpstreamClient) Post(ctx context.Context, baseURL, path string, out, body interface{}) error {
	return c.do(ctx, http.MethodPost, baseURL, path, body, out)
}

// Put 调用上游 PUT 并解包 data 到 out（out 可为 nil）。
func (c *UpstreamClient) Put(ctx context.Context, baseURL, path string, out, body interface{}) error {
	return c.do(ctx, http.MethodPut, baseURL, path, body, out)
}
