package check

import (
	"context"
	"errors"
	"math"
	"os/exec"
	"regexp"
	"strconv"
	"time"
)

// hostPattern is an allow-list, not a sanitiser: the host is handed to a child
// process, and while exec never involves a shell here, a target that cannot be
// a hostname or an IP is a bug in the request rather than something to probe.
var hostPattern = regexp.MustCompile(`^[a-zA-Z0-9.:_-]+$`)

// rttPattern matches both iputils ("time=12.3 ms") and busybox ("time=12.3 ms"),
// as well as the "time<1 ms" a sub-millisecond reply produces.
var rttPattern = regexp.MustCompile(`time[=<]([\d.]+)\s*ms`)

func checkPing(ctx context.Context, c Check, timeout time.Duration) Result {
	host := cleanHost(c.Target)
	if !hostPattern.MatchString(host) {
		return Result{Status: StatusDown, Message: "Invalid host"}
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	seconds := int(timeout / time.Second)
	if seconds < 1 {
		seconds = 1
	}

	// Raw ICMP would need CAP_NET_RAW inside this process; delegating to the
	// system ping keeps the outpost unprivileged and matches what an operator
	// would run by hand. The arguments are passed as a vector, never a shell
	// string, so the host cannot escape into a command.
	//
	// -W is seconds for iputils' ping, which is what the container ships.
	cmd := exec.CommandContext(ctx, "ping", "-c", "1", "-W", strconv.Itoa(seconds), host)
	out, err := cmd.Output()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return Result{Status: StatusDown, Message: "Timeout"}
		}
		return Result{Status: StatusDown, Message: "Host unreachable"}
	}

	if m := rttPattern.FindSubmatch(out); m != nil {
		if rtt, parseErr := strconv.ParseFloat(string(m[1]), 64); parseErr == nil {
			ms := int(math.Round(rtt))
			return Result{Status: StatusUp, Latency: &ms, Message: "Pong"}
		}
	}
	// A reply that we could not parse is still a reply: report up, without a
	// latency, rather than inventing a number.
	return Result{Status: StatusUp, Message: "Pong"}
}
