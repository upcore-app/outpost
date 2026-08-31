// Package check runs the probes an outpost is asked to perform. It is
// stateless: a request carries everything a check needs, and nothing about a
// check survives its result.
package check

import (
	"context"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// The only two status values an outpost ever emits. They are the same integers
// upcore stores in a heartbeat row, so a result can be persisted unchanged.
const (
	StatusDown = 0
	StatusUp   = 1
)

const (
	// defaultTimeout mirrors upcore's default for a monitor that does not
	// carry an explicit one.
	defaultTimeout = 10
	minTimeout     = 1
	maxTimeout     = 120

	// maxHeaders bounds a single HTTP check. upcore validates this too; the
	// limit is repeated here because the outpost is reachable on its own.
	maxHeaders = 20

	// messageLimit keeps a verbose transport error from turning a result into
	// a wall of text in upcore's UI.
	messageLimit = 200
)

// Header is one request header of an http/keyword check.
type Header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Check is one probe as described by upcore. Fields that do not apply to the
// type are ignored rather than rejected, so upcore can send a uniform shape.
type Check struct {
	ID            string   `json:"id"`
	Type          string   `json:"type"`
	Target        string   `json:"target"`
	Timeout       int      `json:"timeout"`
	HTTPMethod    string   `json:"httpMethod"`
	HTTPHeaders   []Header `json:"httpHeaders"`
	Keyword       string   `json:"keyword"`
	Port          int      `json:"port"`
	DNSRecordType string   `json:"dnsRecordType"`
}

// Result is what upcore stores. Latency is a pointer so an unmeasurable probe
// marshals to null instead of a misleading 0 ms.
type Result struct {
	ID      string `json:"id"`
	Status  int    `json:"status"`
	Latency *int   `json:"latency"`
	Message string `json:"message"`
}

// Runner bounds how many probes an outpost has in flight at once, across every
// concurrent request. Without that bound a single large batch of ping checks
// would exhaust the process's file descriptors and slow down every other
// batch's measurements — and a slowed-down measurement is a wrong one.
type Runner struct {
	sem chan struct{}
}

// NewRunner returns a Runner admitting at most maxConcurrency probes at a time.
func NewRunner(maxConcurrency int) *Runner {
	if maxConcurrency < 1 {
		maxConcurrency = 1
	}
	return &Runner{sem: make(chan struct{}, maxConcurrency)}
}

// Run probes every check concurrently and returns the results in request order.
//
// Each goroutine writes to its own index of a pre-sized slice, so there is no
// shared mutable state and no mutex: distinct elements of a slice are distinct
// memory, and the WaitGroup provides the happens-before edge to the caller.
func (r *Runner) Run(ctx context.Context, checks []Check) []Result {
	results := make([]Result, len(checks))

	var wg sync.WaitGroup
	for i, c := range checks {
		wg.Add(1)
		go func(i int, c Check) {
			defer wg.Done()
			// A panic in a probe goroutine would take the whole process with
			// it, past any handler-level recovery, so it is contained here.
			defer func() {
				if rec := recover(); rec != nil {
					results[i] = Result{ID: c.ID, Status: StatusDown, Message: "Interner Fehler"}
				}
			}()

			select {
			case r.sem <- struct{}{}:
			case <-ctx.Done():
				results[i] = Result{ID: c.ID, Status: StatusDown, Message: "Abgebrochen"}
				return
			}
			defer func() { <-r.sem }()

			res := r.probe(ctx, c)
			res.ID = c.ID
			results[i] = res
		}(i, c)
	}
	wg.Wait()

	return results
}

// probe dispatches one check. An unusable descriptor yields a down result
// rather than an error: the batch is a set of independent measurements, and one
// bad row must not cost upcore the other 199.
func (r *Runner) probe(ctx context.Context, c Check) Result {
	c.Target = strings.TrimSpace(c.Target)
	if c.Target == "" {
		return Result{Status: StatusDown, Message: "Empty target"}
	}

	timeout := normalizeTimeout(c.Timeout)

	switch strings.ToLower(strings.TrimSpace(c.Type)) {
	case "http":
		return checkHTTP(ctx, c, timeout)
	case "keyword":
		return checkKeyword(ctx, c, timeout)
	case "ping":
		return checkPing(ctx, c, timeout)
	case "tcp":
		return checkTCP(ctx, c, timeout)
	case "dns":
		return checkDNS(ctx, c, timeout)
	case "ssl":
		return checkSSL(ctx, c, timeout)
	default:
		return Result{Status: StatusDown, Message: "Unsupported check type: " + strings.TrimSpace(c.Type)}
	}
}

// Types lists what this outpost can run; /v1/info advertises it so upcore can
// grey out check types an older outpost does not know.
func Types() []string {
	return []string{"http", "keyword", "ping", "tcp", "dns", "ssl"}
}

func normalizeTimeout(seconds int) time.Duration {
	if seconds <= 0 {
		seconds = defaultTimeout
	}
	if seconds < minTimeout {
		seconds = minTimeout
	}
	if seconds > maxTimeout {
		seconds = maxTimeout
	}
	return time.Duration(seconds) * time.Second
}

// latencyMS rounds to whole milliseconds: upcore stores an integer, and
// sub-millisecond precision is noise over a network path anyway.
func latencyMS(d time.Duration) *int {
	ms := int(d.Round(time.Millisecond) / time.Millisecond)
	if ms < 0 {
		ms = 0
	}
	return &ms
}

func trimMessage(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= messageLimit {
		return s
	}
	cut := s[:messageLimit]
	// Back off to a rune boundary so the JSON stays valid UTF-8.
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut + "…"
}

// cleanHost reduces a monitor target to the bare host the transport-level
// checks dial. Targets are authored for upcore's HTTP checks, so they routinely
// arrive as full URLs even for ping or tcp.
func cleanHost(target string) string {
	host := strings.TrimSpace(target)
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	if i := strings.Index(host, "/"); i >= 0 {
		host = host[:i]
	}
	if i := strings.LastIndex(host, ":"); i > 0 {
		head, tail := host[:i], host[i+1:]
		// Only a trailing all-digit segment is a port, and only when the part
		// before it is not itself full of colons: a bare IPv6 literal such as
		// ::1 must survive untouched, while [::1]:8080 must lose its port.
		if isDigits(tail) && (!strings.Contains(head, ":") || strings.HasSuffix(head, "]")) {
			host = head
		}
	}
	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")
	return host
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
