// Package dnscheck asks specific public resolvers what a name resolves to.
package dnscheck

import (
	"context"
	"net"
	"sort"
	"time"
)

// Lookup returns the IPv4 addresses `resolver` (host:port) gives for name.
func Lookup(ctx context.Context, resolver, name string) ([]string, error) {
	r := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 5 * time.Second}
			return d.DialContext(ctx, "udp", resolver)
		},
	}
	ips, err := r.LookupIP(ctx, "ip4", name)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		out = append(out, ip.String())
	}
	sort.Strings(out)
	return out, nil
}
