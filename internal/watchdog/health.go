package watchdog

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"
)

// Verdict is the outcome of one health check.
type Verdict int

const (
	// Healthy means every check passed.
	Healthy Verdict = iota
	// Unhealthy means the link is genuinely broken and recovery is warranted.
	Unhealthy
	// Unknown means the health of the link could not be established -- the
	// probe could not run, or NetworkManager could not be queried. Recovery
	// must NOT run on Unknown: acting on an inconclusive check is how a
	// watchdog turns a working network into a broken one.
	Unknown
)

func (v Verdict) String() string {
	switch v {
	case Healthy:
		return "healthy"
	case Unhealthy:
		return "unhealthy"
	default:
		return "unknown"
	}
}

// Health is a full diagnosis, kept structured so both the daemon and the
// read-only `check` command can render it.
type Health struct {
	Verdict Verdict
	Reason  string
	Device  DeviceState
	Route   DefaultRoute
	Gateway netip.Addr
}

// CheckConfig configures a health check.
type CheckConfig struct {
	Iface string
	// Gateway overrides the derived default-route nexthop. Leave zero to
	// derive it, which is strongly preferred.
	Gateway     netip.Addr
	PingCount   int
	PingTimeout time.Duration
}

// Check diagnoses the link: NetworkManager must consider the interface
// connected, it must carry an IPv4 default route, and that route's nexthop must
// answer an echo request.
func Check(ctx context.Context, r Runner, p Prober, cfg CheckConfig) Health {
	h := Health{}

	dev, err := QueryDevice(ctx, r, cfg.Iface)
	if err != nil {
		// Could not ask NetworkManager: inconclusive, not broken.
		return Health{Verdict: Unknown, Reason: fmt.Sprintf("cannot query device: %v", err)}
	}
	h.Device = dev
	if !dev.Connected() {
		return Health{Verdict: Unhealthy, Device: dev,
			Reason: fmt.Sprintf("NetworkManager reports %s state=%q", cfg.Iface, dev.State)}
	}

	route, err := QueryDefaultRoute(ctx, r, cfg.Iface)
	if err != nil {
		return Health{Verdict: Unhealthy, Device: dev,
			Reason: fmt.Sprintf("no usable default route: %v", err)}
	}
	h.Route = route

	gw := cfg.Gateway
	if !gw.IsValid() {
		gw = route.Gateway
	}
	h.Gateway = gw
	if !gw.IsValid() {
		// An on-link default route has no nexthop to probe. Treat the
		// presence of a connected device and a default route as good enough
		// rather than inventing a target.
		h.Verdict = Healthy
		h.Reason = fmt.Sprintf("%s connected, on-link default route (no nexthop to probe)", cfg.Iface)
		return h
	}

	if err := p.Probe(gw, cfg.Iface, cfg.PingCount, cfg.PingTimeout); err != nil {
		if errors.Is(err, ErrProbeUnavailable) {
			h.Verdict = Unknown
			h.Reason = fmt.Sprintf("cannot probe gateway %s: %v", gw, err)
			return h
		}
		h.Verdict = Unhealthy
		h.Reason = fmt.Sprintf("gateway %s unreachable: %v", gw, err)
		return h
	}

	h.Verdict = Healthy
	h.Reason = fmt.Sprintf("%s connected, default via %s, gateway answers", cfg.Iface, gw)
	return h
}
