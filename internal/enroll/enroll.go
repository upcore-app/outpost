// Package enroll registers a fresh outpost with upcore, once, using the
// one-time setup key from its deploy command.
//
// This is the single place the outpost talks to upcore rather than the other
// way round, and it is deliberately narrow: one POST, no session, no polling,
// nothing that keeps running afterwards. Everything else stays what it was —
// upcore calls in, the outpost answers.
//
// The handshake is mutual by construction. The outpost proves it holds the
// setup key; upcore proves the outpost is actually reachable by calling
// GET /v1/info with the API key it was just handed, before it writes anything.
// So a probe behind a firewall fails loudly at deploy time instead of quietly
// never being asked for a check.
//
// A successful enrollment leaves a marker in the data dir. The setup key is
// single use, so without it a restarted container would spend its backoff
// budget re-presenting a key upcore has already burned.
package enroll

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// MarkerName records that this data dir already enrolled, and where.
const MarkerName = "enrolled"

const (
	requestTimeout = 30 * time.Second
	// readyTimeout bounds the wait for our own listener: upcore calls back
	// during the enrollment, so announcing ourselves before we serve would
	// fail the very check that is supposed to confirm we work.
	readyTimeout = 20 * time.Second
	// maxErrorBytes is all we quote from an error response; upcore answers with
	// a short JSON object and nothing worth logging is longer.
	maxErrorBytes = 2048
)

// backoff is the delay before each retry. Long enough at the end to survive a
// short upcore outage or a proxy that is still coming up, finite because an
// outpost nobody enrolled is a deploy problem, not something to hide by
// retrying for a day.
var backoff = []time.Duration{
	2 * time.Second,
	5 * time.Second,
	15 * time.Second,
	30 * time.Second,
	60 * time.Second,
	120 * time.Second,
	300 * time.Second,
}

// Options is everything the enrollment needs; it borrows nothing from the
// server so the two can be reasoned about separately.
type Options struct {
	// UpcoreURL is the base URL of the upcore instance, e.g. https://status.example.com
	UpcoreURL string
	// SetupKey is the one-time token from the deploy dialog (ost_…)
	SetupKey string
	// PublicURL is where upcore can reach *this* outpost. Empty means upcore
	// should use the source address of this request together with Port.
	PublicURL string
	// APIKey is the key upcore will present on every later request
	APIKey string
	// Addr is the listen address, used both for the readiness check and to tell
	// upcore which port to come back on
	Addr string
	// DataDir holds the marker file; empty disables it (and thus re-enrolls)
	DataDir string
	Version string
}

// request is the wire format of POST /api/outposts/enroll. It is small on
// purpose: everything descriptive (location, provider, country, version) upcore
// reads from /v1/info during the callback, where it is authenticated.
type request struct {
	Token  string `json:"token"`
	APIKey string `json:"apiKey"`
	URL    string `json:"url,omitempty"`
	Port   int    `json:"port,omitempty"`
	// Addresses are this host's own globally routable addresses, so upcore does
	// not have to guess where to call back. It otherwise falls back to the
	// source address of this request, which behind a CDN or a reverse proxy is
	// the edge that forwarded it rather than this outpost.
	Addresses []string `json:"addresses,omitempty"`
}

type response struct {
	ID      int    `json:"id"`
	Name    string `json:"name"`
	URL     string `json:"url"`
	Adopted bool   `json:"adopted"`
	// Verified is false when upcore registered the outpost but could not reach
	// it back yet — it answers 202 rather than 201 for that. The setup key is
	// spent either way, so this is nothing to retry from here: upcore keeps
	// trying on its side, and what is left to do is on this host. Error says
	// what stopped it.
	//
	// A pointer because an upcore that predates the field sends no `verified`
	// at all, and a plain bool would read that silence as "not verified".
	// submit() fills it in from the status code in that case.
	Verified *bool  `json:"verified"`
	Error    string `json:"error"`
}

// permanentError is a refusal no retry can fix: a wrong, spent or expired key.
type permanentError struct{ msg string }

func (e permanentError) Error() string { return e.msg }

// Configured reports whether this outpost was deployed with auto-enrollment.
// Half a configuration is a mistake worth naming rather than ignoring.
func Configured(opts Options, log *slog.Logger) bool {
	switch {
	case opts.UpcoreURL == "" && opts.SetupKey == "":
		return false
	case opts.UpcoreURL == "":
		log.Warn("OUTPOST_SETUP_KEY is set but OUTPOST_UPCORE_URL is not: not enrolling")
		return false
	case opts.SetupKey == "":
		log.Warn("OUTPOST_UPCORE_URL is set but OUTPOST_SETUP_KEY is not: not enrolling")
		return false
	default:
		return true
	}
}

// Run enrolls the outpost, retrying transient failures until ctx is done or the
// backoff is exhausted. It never returns an error: an outpost that could not
// register is still a working probe, and an admin can always add it by hand.
func Run(ctx context.Context, opts Options, log *slog.Logger) {
	if !Configured(opts, log) {
		return
	}

	marker := markerPath(opts.DataDir)
	if marker != "" {
		if _, err := os.Stat(marker); err == nil {
			log.Info("already enrolled, skipping auto-setup", "marker", marker)
			return
		}
	}

	if err := waitReady(ctx, opts.Addr); err != nil {
		log.Warn("enrolling before the listener was confirmed ready", "error", err)
	}

	for attempt := 0; ; attempt++ {
		result, err := submit(ctx, opts)
		if err == nil {
			log.Info("enrolled with upcore",
				"upcore", opts.UpcoreURL,
				"outpost", result.Name,
				"id", result.ID,
				"url", result.URL,
				"adopted", result.Adopted)
			// Registered but not reached: the key is spent, so retrying here
			// would only burn the backoff on a 409. The fix is on this host,
			// and upcore keeps probing until it lands.
			if result.Verified != nil && !*result.Verified {
				log.Warn("upcore registered this outpost but could not reach it back yet; "+
					"open the port for upcore or set OUTPOST_PUBLIC_URL — upcore keeps retrying",
					"url", result.URL,
					"reason", result.Error)
			}
			writeMarker(marker, opts.UpcoreURL, result, log)
			return
		}

		var permanent permanentError
		if errors.As(err, &permanent) {
			log.Error("upcore refused the setup key, giving up", "error", err)
			return
		}
		if ctx.Err() != nil {
			return
		}
		if attempt >= len(backoff)-1 {
			log.Error("giving up on auto-setup; add this outpost by hand in upcore",
				"error", err, "attempts", attempt+1)
			return
		}

		wait := backoff[attempt]
		log.Warn("enrollment attempt failed, retrying", "error", err, "in", wait.String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func submit(ctx context.Context, opts Options) (response, error) {
	body := request{
		Token:     opts.SetupKey,
		APIKey:    opts.APIKey,
		URL:       strings.TrimRight(strings.TrimSpace(opts.PublicURL), "/"),
		Port:      port(opts.Addr),
		Addresses: localAddresses(),
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return response{}, err
	}

	endpoint := strings.TrimRight(opts.UpcoreURL, "/") + "/api/outposts/enroll"
	reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return response{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "outpost/"+opts.Version)

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return response{}, err
	}
	defer res.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(res.Body, maxErrorBytes))
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		var parsed response
		if err := json.Unmarshal(raw, &parsed); err != nil {
			return response{}, fmt.Errorf("upcore answered %d with an unreadable body: %w", res.StatusCode, err)
		}
		if parsed.Verified == nil {
			// 202 Accepted is upcore saying "registered, but I have not reached
			// you yet". Anything else in the 2xx range means it did.
			verified := res.StatusCode != http.StatusAccepted
			parsed.Verified = &verified
		}
		return parsed, nil
	}

	message := upcoreMessage(raw)
	// 401/409/410 are verdicts about the key itself. Retrying a key upcore has
	// already rejected only fills the log.
	switch res.StatusCode {
	case http.StatusUnauthorized, http.StatusConflict, http.StatusGone:
		return response{}, permanentError{fmt.Sprintf("upcore refused the setup key (%d): %s", res.StatusCode, message)}
	default:
		return response{}, fmt.Errorf("upcore answered %d: %s", res.StatusCode, message)
	}
}

// upcoreMessage digs the human-readable half out of an h3 error body, which is
// JSON with statusMessage/message, and falls back to the raw text.
func upcoreMessage(raw []byte) string {
	var parsed struct {
		StatusMessage string `json:"statusMessage"`
		Message       string `json:"message"`
	}
	if err := json.Unmarshal(raw, &parsed); err == nil {
		if parsed.StatusMessage != "" {
			return parsed.StatusMessage
		}
		if parsed.Message != "" {
			return parsed.Message
		}
	}
	return strings.TrimSpace(string(raw))
}

// waitReady blocks until the outpost's own listener accepts a connection, so
// upcore's callback cannot arrive before there is something to answer it.
func waitReady(ctx context.Context, addr string) error {
	target := dialTarget(addr)
	deadline := time.Now().Add(readyTimeout)
	for {
		conn, err := net.DialTimeout("tcp", target, 2*time.Second)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("listener on %s did not accept within %s", target, readyTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// dialTarget turns a listen address into one that can be dialled: a wildcard
// host is only meaningful to bind, not to connect to.
func dialTarget(addr string) string {
	host, portPart, err := net.SplitHostPort(addr)
	if err != nil {
		return "127.0.0.1:8080"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, portPart)
}

// maxAddresses caps what we report about ourselves. Every entry costs upcore a
// connection attempt inside its verification budget, and a host with more than
// a handful of routable addresses does not become easier to reach by listing
// all of them.
const maxAddresses = 8

// localAddresses returns this host's own globally routable addresses, IPv4
// first: upcore tries them in order, and an IPv4 address is the more likely of
// the two to be reachable from wherever upcore runs.
//
// Only addresses that could be reached from the internet are reported. A probe
// behind NAT sees an RFC 1918 address and a container sees the bridge subnet,
// and neither says anything about where upcore should call back — such a
// deployment needs OUTPOST_PUBLIC_URL, which is a statement rather than a guess.
//
// A failure here is not worth reporting: the list is an optimisation, and upcore
// still has the source address of the enrollment to fall back on.
func localAddresses() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}

	var v4, v6 []string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch value := addr.(type) {
			case *net.IPNet:
				ip = value.IP
			case *net.IPAddr:
				ip = value.IP
			}
			if !routable(ip) {
				continue
			}
			if ip.To4() != nil {
				v4 = append(v4, ip.String())
			} else {
				v6 = append(v6, ip.String())
			}
		}
	}

	all := append(v4, v6...)
	if len(all) > maxAddresses {
		all = all[:maxAddresses]
	}
	return all
}

// routable rules out everything that cannot be reached from somewhere else:
// loopback, link-local, multicast and the unspecified address (IsGlobalUnicast),
// RFC 1918 and unique local addresses (IsPrivate), and carrier-grade NAT, which
// IsPrivate does not cover.
func routable(ip net.IP) bool {
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return false
	}
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
		return false
	}
	return true
}

// port is what upcore appends to the source address when the deployment did not
// name a public URL of its own.
func port(addr string) int {
	_, portPart, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	number, err := strconv.Atoi(portPart)
	if err != nil {
		return 0
	}
	return number
}

func markerPath(dataDir string) string {
	if dataDir == "" {
		return ""
	}
	return filepath.Join(dataDir, MarkerName)
}

// writeMarker is best-effort: a data dir that cannot be written already logged
// loudly when the API key was resolved, and a missing marker costs one refused
// enrollment on the next start, not a broken outpost.
func writeMarker(path, upcoreURL string, result response, log *slog.Logger) {
	if path == "" {
		return
	}
	content := fmt.Sprintf("%s\noutpost id: %d\nname: %s\nurl: %s\nenrolled at: %s\n",
		upcoreURL, result.ID, result.Name, result.URL, time.Now().UTC().Format(time.RFC3339))
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		log.Warn("could not record the enrollment; the next start will try again with a spent key",
			"path", path, "error", err)
	}
}
