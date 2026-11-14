package sensor

import (
	"context"
	"net/http"
	"time"
)

// defaultHTTPGet 默认实现：短超时，避免一个挂掉的接口拖住整个调度循环。
func defaultHTTPGet(ctx context.Context, url string) (int, error) {
	c := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	return resp.StatusCode, nil
}
