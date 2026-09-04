package check

import (
	"context"
	"fmt"
	"io"
	"mime"
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

	// httpProbeBytes is what a plain http check asks the target for. Only the
	// status line decides the verdict, so accepting the representation is pure
	// waste: a monitor pointed at a 40 MB video would otherwise pull those
	// 40 MB once per interval, from every location it runs on.
	httpProbeBytes = 2 << 10

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
	setRange(req, httpProbeBytes)

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return Result{Status: StatusDown, Latency: latencyMS(time.Since(start)), Message: trimMessage(err.Error())}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrain))
	latency := latencyMS(time.Since(start))

	// 416 is the server honouring the Range and having nothing that long to
	// send — an empty representation, not a broken endpoint. It answered.
	if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		return Result{Status: StatusUp, Latency: latency, Message: "416 · Leere Antwort"}
	}

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
	// The reader below caps what is kept; the Range caps what a cooperative
	// server sends in the first place.
	setRange(req, maxKeywordBody)

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return Result{Status: StatusDown, Latency: latencyMS(time.Since(start)), Message: trimMessage(err.Error())}
	}
	defer resp.Body.Close()

	// An error response is down regardless of its body: a 500 page that happens
	// to contain the keyword is still a 500. Decided from the status line, so
	// the body is never accepted.
	if statusFor(resp.StatusCode) == StatusDown {
		latency := latencyMS(time.Since(start))
		// 416 answers the Range with "there is nothing that long": an empty
		// representation, which cannot contain the keyword either.
		if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
			return Result{Status: StatusDown, Latency: latency, Message: "416 · Leere Antwort, Keyword nicht gefunden"}
		}
		return Result{Status: StatusDown, Latency: latency, Message: statusMessage(resp.StatusCode)}
	}

	// A keyword cannot match bytes that are not text. Downloading an image, a
	// video or an installer to search it for a word is megabytes spent on a
	// comparison that can only fail — so this is decided from the header, and
	// the body is dropped unread rather than drained.
	if contentType := resp.Header.Get("Content-Type"); !searchableContentType(contentType) {
		return Result{
			Status:  StatusDown,
			Latency: latencyMS(time.Since(start)),
			Message: fmt.Sprintf("%d · Kein durchsuchbarer Inhalt (%s)", resp.StatusCode, mediaType(contentType)),
		}
	}

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxKeywordBody))
	latency := latencyMS(time.Since(start))
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

// setRange asks for the first bytes only, unless the monitor carries a Range of
// its own. Reading a bounded prefix stops the check from spending the transfer;
// the header stops the server from sending it, and leaves the connection
// reusable instead of closed mid-response.
func setRange(req *http.Request, bytes int) {
	if req.Header.Get("Range") != "" {
		return
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", bytes-1))
}

// mediaType is the type without its parameters, for a message a user can read.
func mediaType(contentType string) string {
	parsed, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return strings.TrimSpace(strings.Split(contentType, ";")[0])
	}
	return parsed
}

// searchableContentType reports whether a keyword could be found in a body of
// this type at all. text/* and the structured application/* types are text;
// everything else — image, video, audio, font, octet-stream, PDF — is not.
// A missing or unparseable Content-Type is not a reason to refuse: plenty of
// endpoints send none, and the byte cap still applies.
func searchableContentType(contentType string) bool {
	if strings.TrimSpace(contentType) == "" {
		return true
	}
	parsed, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return true
	}
	if strings.HasPrefix(parsed, "text/") {
		return true
	}
	subtype, ok := strings.CutPrefix(parsed, "application/")
	if !ok {
		return false
	}
	// application/ld+json, application/atom+xml, … — the suffix is what says
	// how the payload is structured.
	if i := strings.LastIndex(subtype, "+"); i >= 0 {
		subtype = subtype[i+1:]
	}
	switch subtype {
	case "json", "xml", "javascript", "ecmascript", "yaml", "x-yaml", "x-ndjson", "graphql", "x-www-form-urlencoded":
		return true
	}
	return false
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
