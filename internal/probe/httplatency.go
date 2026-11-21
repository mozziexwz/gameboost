package probe

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"time"
)

// HTTPResponseLatency times two HTTP responses through the selected node, using the
// same client so the second request can reuse the connection. CONNECT alone
// never makes a sample successful.
func HTTPResponseLatency(ctx context.Context, proxy Proxy, target string) Result {
	const attempts = 2
	const timeout = 5 * time.Second
	transport := httpProxyTransport(proxy, timeout)
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		Timeout:   timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	samples := make([]Sample, 0, attempts)
	for range attempts {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			samples = append(samples, Sample{Err: "检测 URL 无效"})
			continue
		}
		started := time.Now()
		resp, err := client.Do(req)
		elapsed := time.Since(started)
		if err != nil {
			// The client includes the full URL in url.Error.
			var requestErr *url.Error
			if errors.As(err, &requestErr) {
				err = requestErr.Err
			}
			samples = append(samples, Sample{Err: "HTTP 响应检测失败：" + failureReason(err)})
			continue
		}
		samples = append(samples, Sample{OK: true, Duration: elapsed})
		// Small responses can be drained for keep-alive. The body isn't timed.
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
	}
	result := NewResult(samples)
	result.fastest = true
	return result
}
