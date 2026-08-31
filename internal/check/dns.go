package check

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

// The record types upcore offers in its monitor form.
var dnsRecordTypes = map[string]bool{
	"A":     true,
	"AAAA":  true,
	"CNAME": true,
	"MX":    true,
	"NS":    true,
	"TXT":   true,
}

func checkDNS(ctx context.Context, c Check, timeout time.Duration) Result {
	host := cleanHost(c.Target)
	if host == "" {
		return Result{Status: StatusDown, Message: "Invalid host"}
	}

	recordType := strings.ToUpper(strings.TrimSpace(c.DNSRecordType))
	if recordType == "" {
		recordType = "A"
	}
	if !dnsRecordTypes[recordType] {
		return Result{Status: StatusDown, Message: "Unsupported DNS record type: " + recordType}
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// The zero Resolver uses the host's configured nameservers, which is what
	// an operator expects from a probe placed in a specific location.
	var resolver net.Resolver

	start := time.Now()
	records, err := lookup(ctx, &resolver, recordType, host)
	latency := latencyMS(time.Since(start))

	switch {
	case err != nil:
		return Result{Status: StatusDown, Latency: latency, Message: trimMessage(err.Error())}
	case len(records) == 0:
		return Result{Status: StatusDown, Latency: latency, Message: "Kein " + recordType + "-Record"}
	}

	return Result{
		Status:  StatusUp,
		Latency: latency,
		Message: trimMessage(fmt.Sprintf("%d × %s: %s", len(records), recordType, records[0])),
	}
}

// lookup returns the records as text, one entry per record, in resolver order.
// recordType is already known to be supported.
func lookup(ctx context.Context, resolver *net.Resolver, recordType, host string) ([]string, error) {
	switch recordType {
	case "A", "AAAA":
		network := "ip4"
		if recordType == "AAAA" {
			network = "ip6"
		}
		// LookupIP's network argument does the family filtering, so a host with
		// only A records reports zero AAAA instead of its IPv4 set.
		ips, err := resolver.LookupIP(ctx, network, host)
		if err != nil {
			return nil, err
		}
		out := make([]string, 0, len(ips))
		for _, ip := range ips {
			out = append(out, ip.String())
		}
		return out, nil

	case "CNAME":
		cname, err := resolver.LookupCNAME(ctx, host)
		if err != nil {
			return nil, err
		}
		cname = strings.TrimSpace(cname)
		if cname == "" {
			return nil, nil
		}
		return []string{cname}, nil

	case "MX":
		mxs, err := resolver.LookupMX(ctx, host)
		if err != nil {
			return nil, err
		}
		out := make([]string, 0, len(mxs))
		for _, mx := range mxs {
			out = append(out, fmt.Sprintf("%d %s", mx.Pref, mx.Host))
		}
		return out, nil

	case "NS":
		nss, err := resolver.LookupNS(ctx, host)
		if err != nil {
			return nil, err
		}
		out := make([]string, 0, len(nss))
		for _, ns := range nss {
			out = append(out, ns.Host)
		}
		return out, nil

	default: // TXT
		return resolver.LookupTXT(ctx, host)
	}
}
