package watchdog

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
)

// DefaultRoute is an IPv4 default route on a specific interface.
type DefaultRoute struct {
	Gateway netip.Addr // zero when the default route is on-link
	Device  string
	Metric  int
	Line    string
}

// QueryDefaultRoute returns the IPv4 default route bound to iface.
//
// The gateway is DERIVED here rather than configured. A hardcoded gateway is
// the single most dangerous thing a watchdog like this can carry: when the LAN
// is renumbered the address goes stale, the reachability probe can never
// succeed, and the watchdog bounces a perfectly healthy link forever. Deriving
// also keeps the probe correct when the default route legitimately points at
// something other than the ISP router (a policy-routing gateway, say).
func QueryDefaultRoute(ctx context.Context, r Runner, iface string) (DefaultRoute, error) {
	out, err := r.Run(ctx, "ip", "-4", "route", "show", "default", "dev", iface)
	if err != nil {
		return DefaultRoute{}, fmt.Errorf("ip route show default dev %s: %w", iface, err)
	}
	routes := parseDefaultRoutes(out)
	if len(routes) == 0 {
		return DefaultRoute{}, fmt.Errorf("no IPv4 default route on %s", iface)
	}
	// Lowest metric wins, matching the kernel's own preference.
	best := routes[0]
	for _, rt := range routes[1:] {
		if rt.Metric < best.Metric {
			best = rt
		}
	}
	return best, nil
}

func parseDefaultRoutes(out string) []DefaultRoute {
	var routes []DefaultRoute
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 || f[0] != "default" {
			continue
		}
		rt := DefaultRoute{Line: strings.TrimSpace(line)}
		for i := 0; i < len(f)-1; i++ {
			switch f[i] {
			case "via":
				if a, err := netip.ParseAddr(f[i+1]); err == nil && a.Is4() {
					rt.Gateway = a
				}
			case "dev":
				rt.Device = f[i+1]
			case "metric":
				rt.Metric = atoiDefault(f[i+1], 0)
			}
		}
		routes = append(routes, rt)
	}
	return routes
}

func atoiDefault(s string, def int) int {
	n := 0
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return def
		}
		n = n*10 + int(ch-'0')
	}
	if s == "" {
		return def
	}
	return n
}
