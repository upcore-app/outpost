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

	deadline, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	dialer := &net.Dialer{Timeout: timeout}
	if d, ok := deadline.Deadline(); ok {
		dialer.Deadline = d
	}

	start := time.Now()
	// InsecureSkipVerify hands us the certificate the server actually presents
	// instead of an error: the whole point of this check is to *report* why a
	// certificate is bad and how long a good one has left, which requires
	// holding the chain. The verification below is the real verdict — nothing
	// is trusted just because the handshake completed.
	conn, err := tls.DialWithDialer(dialer, "tcp", net.JoinHostPort(host, strconv.Itoa(port)), &tls.Config{
		InsecureSkipVerify: true,
		ServerName:         host,
	})
	if err != nil {
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return Result{Status: StatusDown, Message: "Timeout"}
		}
		return Result{Status: StatusDown, Message: trimMessage(err.Error())}
	}
	latency := latencyMS(time.Since(start))
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
	// A nil Roots pool means the system pool, which is what we want; asking for
	// it explicitly only lets a broken store surface as an error here.
	roots, err := x509.SystemCertPool()
	if err != nil {
		roots = nil
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		DNSName:       host,
		Intermediates: intermediates,
		Roots:         roots,
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
