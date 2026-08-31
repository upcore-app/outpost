package enroll

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// listening stands in for the outpost's own server, which Run waits for before
// it announces an address upcore is going to call back.
func listening(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	return listener.Addr().String()
}

func TestConfigured(t *testing.T) {
	cases := []struct {
		name string
		opts Options
		want bool
	}{
		{"neither", Options{}, false},
		{"both", Options{UpcoreURL: "https://upcore.test", SetupKey: "ost_x"}, true},
		{"key only", Options{SetupKey: "ost_x"}, false},
		{"url only", Options{UpcoreURL: "https://upcore.test"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Configured(tc.opts, quietLogger()); got != tc.want {
				t.Fatalf("Configured() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDialTargetAndPort(t *testing.T) {
	cases := []struct {
		addr   string
		target string
		port   int
	}{
		{":8080", "127.0.0.1:8080", 8080},
		{"0.0.0.0:9000", "127.0.0.1:9000", 9000},
		{"[::]:8080", "127.0.0.1:8080", 8080},
		{"10.0.0.4:8080", "10.0.0.4:8080", 8080},
		{"nonsense", "127.0.0.1:8080", 0},
	}
	for _, tc := range cases {
		if got := dialTarget(tc.addr); got != tc.target {
			t.Errorf("dialTarget(%q) = %q, want %q", tc.addr, got, tc.target)
		}
		if got := port(tc.addr); got != tc.port {
			t.Errorf("port(%q) = %d, want %d", tc.addr, got, tc.port)
		}
	}
}

func TestUpcoreMessage(t *testing.T) {
	cases := []struct{ body, want string }{
		{`{"statusMessage":"Ungültiger Setup-Key"}`, "Ungültiger Setup-Key"},
		{`{"message":"boom"}`, "boom"},
		{"plain text\n", "plain text"},
	}
	for _, tc := range cases {
		if got := upcoreMessage([]byte(tc.body)); got != tc.want {
			t.Errorf("upcoreMessage(%q) = %q, want %q", tc.body, got, tc.want)
		}
	}
}

// The happy path end to end against a stand-in upcore: the payload carries the
// token and the key, and the marker is written so a restart does not try again.
func TestRunEnrollsOnceAndMarks(t *testing.T) {
	var calls int
	var received request
	upcore := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/api/outposts/enroll" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&received)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(response{ID: 7, Name: "Frankfurt", URL: "http://127.0.0.1:8080"})
	}))
	defer upcore.Close()

	dir := t.TempDir()
	opts := Options{
		UpcoreURL: upcore.URL,
		SetupKey:  "ost_abcdef01_" + "0123456789abcdef0123456789abcdef0123456789abcdef",
		APIKey:    "opk_abcdef01_0123456789abcdef0123456789abcdef0123456789abcdef",
		Addr:      listening(t),
		DataDir:   dir,
		Version:   "test",
	}

	Run(context.Background(), opts, quietLogger())

	if calls != 1 {
		t.Fatalf("upcore was called %d times, want 1", calls)
	}
	if received.Token != opts.SetupKey || received.APIKey != opts.APIKey {
		t.Fatalf("unexpected payload: %+v", received)
	}
	if _, err := os.Stat(filepath.Join(dir, MarkerName)); err != nil {
		t.Fatalf("marker not written: %v", err)
	}

	// A second start finds the marker and stays quiet.
	Run(context.Background(), opts, quietLogger())
	if calls != 1 {
		t.Fatalf("upcore was called again after the marker existed (%d calls)", calls)
	}
}

// A refused key is final: one attempt, no backoff, no marker.
func TestRunGivesUpOnRefusedKey(t *testing.T) {
	var calls int
	upcore := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"statusMessage":"Dieser Setup-Key wurde bereits verwendet"}`))
	}))
	defer upcore.Close()

	dir := t.TempDir()
	Run(context.Background(), Options{
		UpcoreURL: upcore.URL,
		SetupKey:  "ost_abcdef01_0123456789abcdef0123456789abcdef0123456789abcdef",
		APIKey:    "opk_abcdef01_0123456789abcdef0123456789abcdef0123456789abcdef",
		Addr:      listening(t),
		DataDir:   dir,
	}, quietLogger())

	if calls != 1 {
		t.Fatalf("upcore was called %d times, want 1", calls)
	}
	if _, err := os.Stat(filepath.Join(dir, MarkerName)); !os.IsNotExist(err) {
		t.Fatalf("a refused enrollment must not leave a marker (err=%v)", err)
	}
}

// The addresses reported to upcore decide where it calls back, so the filter
// that produces them is worth pinning down: anything unroutable in the list
// only spends one of upcore's connection attempts on an address that cannot
// answer.
func TestRoutable(t *testing.T) {
	cases := []struct {
		address string
		want    bool
	}{
		{"62.238.8.178", true},
		{"2a01:4f9:c015:1441::1", true},
		{"8.8.8.8", true},
		{"10.0.0.5", false},        // RFC 1918
		{"172.16.3.1", false},      // RFC 1918
		{"192.168.1.10", false},    // RFC 1918
		{"100.64.0.1", false},      // carrier-grade NAT
		{"100.127.255.254", false}, // carrier-grade NAT, upper edge
		{"100.128.0.1", true},      // just outside 100.64.0.0/10
		{"127.0.0.1", false},       // loopback
		{"169.254.10.1", false},    // link-local
		{"0.0.0.0", false},         // unspecified
		{"224.0.0.1", false},       // multicast
		{"::1", false},             // loopback
		{"fe80::1", false},         // link-local
		{"fd00::1", false},         // unique local
		{"ff02::1", false},         // multicast
	}

	for _, tc := range cases {
		ip := net.ParseIP(tc.address)
		if ip == nil {
			t.Fatalf("%s does not parse", tc.address)
		}
		if got := routable(ip); got != tc.want {
			t.Errorf("routable(%s) = %v, want %v", tc.address, got, tc.want)
		}
	}
}

// A nil address reaches routable() whenever an interface hands back an address
// type localAddresses does not know, so it must not panic.
func TestRoutableNil(t *testing.T) {
	if routable(nil) {
		t.Fatal("a nil address must not be reported")
	}
}

// localAddresses runs against whatever interfaces the test machine has, so it
// can only assert the invariant: everything it returns is routable, and the
// list is capped.
func TestLocalAddressesAreRoutable(t *testing.T) {
	addresses := localAddresses()
	if len(addresses) > maxAddresses {
		t.Fatalf("returned %d addresses, cap is %d", len(addresses), maxAddresses)
	}
	for _, address := range addresses {
		ip := net.ParseIP(address)
		if ip == nil {
			t.Fatalf("%q is not a parseable address", address)
		}
		if !routable(ip) {
			t.Errorf("%s is not routable but was reported", address)
		}
	}
}
