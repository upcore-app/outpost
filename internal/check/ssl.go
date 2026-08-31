package check

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"
)

const defaultTLSPort = 443

func checkSSL(ctx context.Context, c Check, timeout time.Duration) Result {
	host := cleanHost(c.Target)
	if host == "" {
		return Result{Status: StatusDown, Message: "Invalid host"}
	}
	port := c.Port
	if port <= 0 {
		port = defaultTLSPort
	}
	if port > 65535 {
		return Result{Status: StatusDown, Message: "Kein gültiger Port angegeben"}
	}

	// tls.Dialer over DialWithDialer: the context bounds the handshake as well
	// as the dial, so a batch abandoned by upcore stops here instead of holding
	// a socket open for the full timeout.
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	dialer := &tls.Dialer{
		NetDialer: &net.Dialer{},
		// InsecureSkipVerify hands us the certificate the server actually
		// presents instead of an error: the whole point of this check is to
		// *report* why a certificate is bad and how long a good one has left,
		// which requires holding the chain. The verification below is the real
		// verdict — nothing is trusted just because the handshake completed.
		Config: &tls.Config{
			InsecureSkipVerify: true,
			ServerName:         host,
		},
	}

	start := time.Now()
	raw, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return Result{Status: StatusDown, Message: "Timeout"}
		}
		return Result{Status: StatusDown, Message: trimMessage(err.Error())}
	}
	latency := latencyMS(time.Since(start))
	// tls.Dialer documents that a successful DialContext always yields a
	// *tls.Conn, which is the only way to reach the peer's certificates.
	conn := raw.(*tls.Conn)
	defer conn.Close()

	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return Result{Status: StatusDown, Latency: latency, Message: "Kein Zertifikat erhalten"}
	}

	leaf := certs[0]
	now := time.Now()
	// Expiry is reported on its own because it is the failure an operator acts
	// on, and "certificate has expired" buried in a chain error is easy to miss.
	if now.After(leaf.NotAfter) {
		return Result{Status: StatusDown, Latency: latency, Message: "Zertifikat abgelaufen"}
	}

	intermediates := x509.NewCertPool()
	for _, cert := range certs[1:] {
		intermediates.AddCert(cert)
	}
	// Roots is left nil on purpose: Verify then uses the system pool, and
	// reports a broken or empty store as a verification error of its own rather
	// than one this code would have to invent a message for.
	if _, err := leaf.Verify(x509.VerifyOptions{
		DNSName:       host,
		Intermediates: intermediates,
		CurrentTime:   now,
	}); err != nil {
		return Result{Status: StatusDown, Latency: latency, Message: trimMessage("Zertifikat ungültig: " + err.Error())}
	}

	days := int(leaf.NotAfter.Sub(now).Hours() / 24)
	return Result{
		Status:  StatusUp,
		Latency: latency,
		Message: fmt.Sprintf("Gültig, läuft in %d Tagen ab", days),
	}
}
