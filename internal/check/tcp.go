package check

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"
)

func checkTCP(ctx context.Context, c Check, timeout time.Duration) Result {
	if c.Port <= 0 || c.Port > 65535 {
		return Result{Status: StatusDown, Message: "Kein gültiger Port angegeben"}
	}
	host := cleanHost(c.Target)
	if host == "" {
		return Result{Status: StatusDown, Message: "Invalid host"}
	}

	// The dialer carries the per-check timeout; the context carries the batch
	// deadline, so whichever expires first ends the dial.
	dialer := net.Dialer{Timeout: timeout}
	start := time.Now()
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(c.Port)))
	if err != nil {
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return Result{Status: StatusDown, Message: "Timeout"}
		}
		return Result{Status: StatusDown, Message: trimMessage(err.Error())}
	}
	latency := latencyMS(time.Since(start))
	conn.Close()

	return Result{
		Status:  StatusUp,
		Latency: latency,
		Message: fmt.Sprintf("Port %d offen", c.Port),
	}
}
