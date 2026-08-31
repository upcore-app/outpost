package check

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// maxDrain caps what a plain http check reads. The body does not influence
	// the verdict, but it has to be drained before the connection can be
	// reused, and an endpoint streaming megabytes must not stall the batch.
	maxDrain = 64 << 10

	// maxKeywordBody caps the body a keyword check inspects. A status page
	// large enough to exceed this is not the kind of document a keyword is
	// hidden in.
	maxKeywordBody = 2 << 20

	userAgent = "upcore-outpost"
)

var allowedMethods = map[string]bool{
	http.MethodGet:     true,
	http.MethodHead:    true,
	http.MethodPost:    true,
	http.MethodPut:     true,
	http.MethodPatch:   true,
	http.MethodDelete:  true,
	http.MethodOptions: true,
}

func checkHTTP(ctx context.Context, c Check, timeout time.Duration) Result {
	req, client, err := httpRequest(ctx, c, timeout)
	if err != nil {
		return Result{Status: StatusDown, Message: trimMessage(err.Error())}
	}

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return Result{Status: StatusDown, Latency: latencyMS(time.Since(start)), Message: trimMessage(err.Error())}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrain))
	latency := latencyMS(time.Since(start))

	return Result{Status: statusFor(resp.StatusCode), Latency: latency, Message: statusMessage(resp.StatusCode)}
}

func checkKeyword(ctx context.Context, c Check, timeout time.Duration) Result {
	keyword := strings.TrimSpace(c.Keyword)
	if keyword == "" {
		return Result{Status: StatusDown, Message: "Kein Keyword angegeben"}
	}

	req, client, err := httpRequest(ctx, c, timeout)
	if err != nil {
		return Result{Status: StatusDown, Message: trimMessage(err.Error())}
	}

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return Result{Status: StatusDown, Latency: latencyMS(time.Since(start)), Message: trimMessage(err.Error())}
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxKeywordBody))
	latency := latencyMS(time.Since(start))

	// An error response is down regardless of its body: a 500 page that happens
	// to contain the keyword is still a 500.
	if statusFor(resp.StatusCode) == StatusDown {
		return Result{Status: StatusDown, Latency: latency, Message: statusMessage(resp.StatusCode)}
	}
	if readErr != nil {
		return Result{Status: StatusDown, Latency: latency, Message: trimMessage(readErr.Error())}
	}

	if strings.Contains(string(body), keyword) {
		return Result{
			Status:  StatusUp,
			Latency: latency,
			Message: fmt.Sprintf("%d · Keyword gefunden", resp.StatusCode),
		}
	}
	return Result{
		Status:  StatusDown,
		Latency: latency,
		Message: fmt.Sprintf("%d · Keyword \"%s\" nicht gefunden", resp.StatusCode, keyword),
	}
}

// httpRequest builds the request and the client both HTTP-shaped checks use.
func httpRequest(ctx context.Context, c Check, timeout time.Duration) (*http.Request, *http.Client, error) {
	method := strings.ToUpper(strings.TrimSpace(c.HTTPMethod))
	if method == "" {
		method = http.MethodGet
	}
	if !allowedMethods[method] {
		return nil, nil, fmt.Errorf("Unsupported HTTP method: %s", method)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.Target, nil)
	if err != nil {
		return nil, nil, err
	}

	for i, h := range c.HTTPHeaders {
		if i >= maxHeaders {
			break
		}
		name := strings.TrimSpace(h.Name)
		if name == "" {
			continue
		}
		// A CR or LF in a value is a header-injection attempt (or a mangled
		// paste). Dropping the header keeps the check running against the
		// intended target instead of failing it for a reason the user cannot
		// see in upcore.
		if strings.ContainsAny(h.Value, "\r\n") {
			continue
		}
		req.Header.Set(name, h.Value)
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", userAgent)
	}

	// Redirects are followed (stdlib default: up to 10 hops) because upcore
	// judges the endpoint a user would reach, not the first hop.
	client := &http.Client{Timeout: timeout}
	return req, client, nil
}

// statusFor is upcore's own rule: 2xx and 3xx are up, everything else is down.
func statusFor(code int) int {
	if code >= 200 && code < 400 {
		return StatusUp
	}
	return StatusDown
}

func statusMessage(code int) string {
	return strings.TrimSpace(fmt.Sprintf("%d %s", code, http.StatusText(code)))
}
