package check

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestCleanHost(t *testing.T) {
	cases := []struct {
		name   string
		target string
		want   string
	}{
		{"bare host", "example.com", "example.com"},
		{"url with path", "https://example.com/health?deep=1", "example.com"},
		{"url with trailing slash", "https://example.com/", "example.com"},
		{"host and port", "example.com:8443", "example.com"},
		{"scheme host port path", "tcp://db.internal:5432/ignored", "db.internal"},
		{"ipv4 with port", "192.0.2.10:443", "192.0.2.10"},
		{"bare ipv6 literal", "2001:db8::1", "2001:db8::1"},
		{"bracketed ipv6 with port", "[2001:db8::1]:8443", "2001:db8::1"},
		{"bracketed ipv6 without port", "[2001:db8::1]", "2001:db8::1"},
		{"url with bracketed ipv6", "https://[2001:db8::1]:8443/status", "2001:db8::1"},
		{"padded", "  example.com  ", "example.com"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cleanHost(tc.target); got != tc.want {
				t.Errorf("cleanHost(%q) = %q, want %q", tc.target, got, tc.want)
			}
		})
	}
}

func TestNormalizeTimeout(t *testing.T) {
	cases := []struct {
		seconds int
		want    time.Duration
	}{
		{0, 10 * time.Second},
		{-5, 10 * time.Second},
		{1, 1 * time.Second},
		{30, 30 * time.Second},
		{999, 120 * time.Second},
	}

	for _, tc := range cases {
		if got := normalizeTimeout(tc.seconds); got != tc.want {
			t.Errorf("normalizeTimeout(%d) = %s, want %s", tc.seconds, got, tc.want)
		}
	}
}

func TestRunHTTP(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("fine"))
	}))
	defer ok.Close()

	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer broken.Close()

	cases := []struct {
		name        string
		check       Check
		wantStatus  int
		wantMessage string
	}{
		{
			name:        "healthy endpoint is up",
			check:       Check{ID: "1", Type: "http", Target: ok.URL, Timeout: 5},
			wantStatus:  StatusUp,
			wantMessage: "200 OK",
		},
		{
			name:        "server error is down",
			check:       Check{ID: "2", Type: "http", Target: broken.URL, Timeout: 5},
			wantStatus:  StatusDown,
			wantMessage: "500 Internal Server Error",
		},
		{
			name:        "explicit method is honoured",
			check:       Check{ID: "3", Type: "http", Target: ok.URL, HTTPMethod: "head", Timeout: 5},
			wantStatus:  StatusUp,
			wantMessage: "200 OK",
		},
		{
			name:        "unknown method fails the check only",
			check:       Check{ID: "4", Type: "http", Target: ok.URL, HTTPMethod: "TRACE", Timeout: 5},
			wantStatus:  StatusDown,
			wantMessage: "Unsupported HTTP method: TRACE",
		},
	}

	runner := NewRunner(4)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			results := runner.Run(context.Background(), []Check{tc.check})
			if len(results) != 1 {
				t.Fatalf("got %d results, want 1", len(results))
			}
			got := results[0]
			if got.ID != tc.check.ID {
				t.Errorf("id = %q, want %q", got.ID, tc.check.ID)
			}
			if got.Status != tc.wantStatus {
				t.Errorf("status = %d, want %d", got.Status, tc.wantStatus)
			}
			if got.Message != tc.wantMessage {
				t.Errorf("message = %q, want %q", got.Message, tc.wantMessage)
			}
		})
	}
}

func TestRunHTTPMeasuresLatency(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("fine"))
	}))
	defer ts.Close()

	results := NewRunner(1).Run(context.Background(), []Check{{ID: "1", Type: "http", Target: ts.URL, Timeout: 5}})
	if results[0].Latency == nil {
		t.Fatal("latency is nil, want a measurement")
	}
	if *results[0].Latency < 0 {
		t.Errorf("latency = %d, want >= 0", *results[0].Latency)
	}
}

// The point of the Range header is traffic: a monitor must not pull the whole
// representation once per interval just to read a status line.
func TestRunHTTPAsksForAPrefixOnly(t *testing.T) {
	var seen string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Range")
		w.WriteHeader(http.StatusPartialContent)
	}))
	defer ts.Close()

	got := NewRunner(1).Run(context.Background(), []Check{{ID: "1", Type: "http", Target: ts.URL, Timeout: 5}})[0]
	if seen != "bytes=0-2047" {
		t.Errorf("Range = %q, want %q", seen, "bytes=0-2047")
	}
	if got.Status != StatusUp {
		t.Errorf("status = %d, want %d", got.Status, StatusUp)
	}
}

// A monitor's own Range wins, and a server that refuses ours still answered.
func TestRunHTTPRangeEdges(t *testing.T) {
	var seen string
	custom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Range")
		w.WriteHeader(http.StatusOK)
	}))
	defer custom.Close()

	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
	}))
	defer empty.Close()

	runner := NewRunner(2)
	got := runner.Run(context.Background(), []Check{{
		ID: "1", Type: "http", Target: custom.URL, Timeout: 5,
		HTTPHeaders: []Header{{Name: "Range", Value: "bytes=0-99"}},
	}})[0]
	if seen != "bytes=0-99" {
		t.Errorf("Range = %q, want the monitor's own %q", seen, "bytes=0-99")
	}
	if got.Status != StatusUp {
		t.Errorf("status = %d, want %d", got.Status, StatusUp)
	}

	got = runner.Run(context.Background(), []Check{{ID: "2", Type: "http", Target: empty.URL, Timeout: 5}})[0]
	if got.Status != StatusUp {
		t.Errorf("416 status = %d, want %d", got.Status, StatusUp)
	}
	if got.Message != "416 · Leere Antwort" {
		t.Errorf("416 message = %q", got.Message)
	}
}

func TestSearchableContentType(t *testing.T) {
	cases := map[string]bool{
		"":                                  true,
		"text/html; charset=utf-8":          true,
		"text/plain":                        true,
		"application/json":                  true,
		"application/ld+json":               true,
		"application/atom+xml":              true,
		"application/x-www-form-urlencoded": true,
		"not a media type":                  true,
		"video/mp4":                         false,
		"image/png":                         false,
		"audio/mpeg":                        false,
		"font/woff2":                        false,
		"application/pdf":                   false,
		"application/octet-stream":          false,
		"application/zip":                   false,
	}

	for contentType, want := range cases {
		if got := searchableContentType(contentType); got != want {
			t.Errorf("searchableContentType(%q) = %v, want %v", contentType, got, want)
		}
	}
}

func TestRunKeyword(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"state":"ok","queue":0}`))
	}))
	defer ts.Close()

	// Answers with a media type no keyword can live in. The body must never be
	// read, so writing it here would be a lie — the header is the whole test.
	video := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		w.WriteHeader(http.StatusOK)
	}))
	defer video.Close()

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"state":"ok"}`))
	}))
	defer failing.Close()

	cases := []struct {
		name        string
		check       Check
		wantStatus  int
		wantMessage string
	}{
		{
			name:        "keyword found",
			check:       Check{ID: "1", Type: "keyword", Target: ts.URL, Keyword: "ok", Timeout: 5},
			wantStatus:  StatusUp,
			wantMessage: "200 · Keyword gefunden",
		},
		{
			name:        "keyword missing",
			check:       Check{ID: "2", Type: "keyword", Target: ts.URL, Keyword: "degraded", Timeout: 5},
			wantStatus:  StatusDown,
			wantMessage: `200 · Keyword "degraded" nicht gefunden`,
		},
		{
			name:        "error response wins over the body",
			check:       Check{ID: "3", Type: "keyword", Target: failing.URL, Keyword: "ok", Timeout: 5},
			wantStatus:  StatusDown,
			wantMessage: "503 Service Unavailable",
		},
		{
			name:        "missing keyword is a down result",
			check:       Check{ID: "4", Type: "keyword", Target: ts.URL, Timeout: 5},
			wantStatus:  StatusDown,
			wantMessage: "Kein Keyword angegeben",
		},
		{
			name:        "a media body is refused before it is read",
			check:       Check{ID: "5", Type: "keyword", Target: video.URL, Keyword: "ok", Timeout: 5},
			wantStatus:  StatusDown,
			wantMessage: "200 · Kein durchsuchbarer Inhalt (video/mp4)",
		},
	}

	runner := NewRunner(4)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runner.Run(context.Background(), []Check{tc.check})[0]
			if got.Status != tc.wantStatus {
				t.Errorf("status = %d, want %d", got.Status, tc.wantStatus)
			}
			if got.Message != tc.wantMessage {
				t.Errorf("message = %q, want %q", got.Message, tc.wantMessage)
			}
		})
	}
}

func TestRunRejectsUnusableChecks(t *testing.T) {
	cases := []struct {
		name        string
		check       Check
		wantMessage string
	}{
		{
			name:        "unsupported type",
			check:       Check{ID: "1", Type: "gopher", Target: "example.com"},
			wantMessage: "Unsupported check type: gopher",
		},
		{
			name:        "empty type",
			check:       Check{ID: "2", Type: "", Target: "example.com"},
			wantMessage: "Unsupported check type: ",
		},
		{
			name:        "empty target",
			check:       Check{ID: "3", Type: "http", Target: "   "},
			wantMessage: "Empty target",
		},
		{
			name:        "ping to a host that cannot be one",
			check:       Check{ID: "4", Type: "ping", Target: "https://exa mple.com/x"},
			wantMessage: "Invalid host",
		},
		{
			name:        "tcp without a port",
			check:       Check{ID: "5", Type: "tcp", Target: "example.com"},
			wantMessage: "Kein gültiger Port angegeben",
		},
		{
			name:        "unsupported dns record type",
			check:       Check{ID: "6", Type: "dns", Target: "example.com", DNSRecordType: "SRV"},
			wantMessage: "Unsupported DNS record type: SRV",
		},
	}

	runner := NewRunner(4)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runner.Run(context.Background(), []Check{tc.check})[0]
			if got.Status != StatusDown {
				t.Errorf("status = %d, want %d", got.Status, StatusDown)
			}
			if got.Latency != nil {
				t.Errorf("latency = %d, want nil", *got.Latency)
			}
			if got.Message != tc.wantMessage {
				t.Errorf("message = %q, want %q", got.Message, tc.wantMessage)
			}
		})
	}
}

// The order of the results is part of the wire contract: upcore pairs them with
// its request positionally as well as by id.
func TestRunPreservesOrder(t *testing.T) {
	checks := make([]Check, 25)
	for i := range checks {
		checks[i] = Check{ID: string(rune('a' + i)), Type: "gopher", Target: "example.com"}
	}

	results := NewRunner(4).Run(context.Background(), checks)
	if len(results) != len(checks) {
		t.Fatalf("got %d results, want %d", len(results), len(checks))
	}
	for i, res := range results {
		if res.ID != checks[i].ID {
			t.Errorf("results[%d].id = %q, want %q", i, res.ID, checks[i].ID)
		}
	}
}
