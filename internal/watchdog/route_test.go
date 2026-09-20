package watchdog

import (
	"context"
	"strings"
	"testing"
)

const (
	routeViaISP   = "default via 192.168.8.1 dev wlan0 proto dhcp src 192.168.8.224 metric 600 \n"
	routeViaGotun = "default via 192.168.8.162 dev wlan0 proto static metric 600 \n"
)

func routeRunner(iface, out string) *RecordingRunner {
	r := NewRecordingRunner()
	r.Outputs["ip -4 route show default dev "+iface] = out
	return r
}

func TestQueryDefaultRoute_DerivesGateway(t *testing.T) {
	rt, err := QueryDefaultRoute(context.Background(), routeRunner("wlan0", routeViaISP), "wlan0")
	if err != nil {
		t.Fatalf("QueryDefaultRoute: %v", err)
	}
	if rt.Gateway.String() != "192.168.8.1" || rt.Device != "wlan0" || rt.Metric != 600 {
		t.Fatalf("got %+v", rt)
	}
}

// Deriving is the point: when the default route is moved to a policy-routing
// gateway, the probe target must follow it automatically.
func TestQueryDefaultRoute_FollowsAChangedGateway(t *testing.T) {
	rt, err := QueryDefaultRoute(context.Background(), routeRunner("wlan0", routeViaGotun), "wlan0")
	if err != nil {
		t.Fatalf("QueryDefaultRoute: %v", err)
	}
	if rt.Gateway.String() != "192.168.8.162" {
		t.Fatalf("gateway = %s, want the current nexthop", rt.Gateway)
	}
}

func TestQueryDefaultRoute_NoDefaultRouteErrors(t *testing.T) {
	_, err := QueryDefaultRoute(context.Background(), routeRunner("wlan0", "\n"), "wlan0")
	if err == nil || !strings.Contains(err.Error(), "no IPv4 default route") {
		t.Fatalf("err = %v", err)
	}
}

func TestQueryDefaultRoute_PrefersLowestMetric(t *testing.T) {
	out := "default via 192.168.8.1 dev wlan0 metric 600\ndefault via 192.168.8.9 dev wlan0 metric 100\n"
	rt, err := QueryDefaultRoute(context.Background(), routeRunner("wlan0", out), "wlan0")
	if err != nil {
		t.Fatalf("QueryDefaultRoute: %v", err)
	}
	if rt.Gateway.String() != "192.168.8.9" {
		t.Fatalf("gateway = %s, want the lowest-metric nexthop", rt.Gateway)
	}
}

func TestParseDefaultRoutes_OnLinkHasNoGateway(t *testing.T) {
	routes := parseDefaultRoutes("default dev wlan0 scope link metric 600\n")
	if len(routes) != 1 {
		t.Fatalf("got %d routes", len(routes))
	}
	if routes[0].Gateway.IsValid() {
		t.Fatalf("on-link default should have no nexthop, got %s", routes[0].Gateway)
	}
}

func TestParseDefaultRoutes_IgnoresNonDefaultLines(t *testing.T) {
	out := "192.168.8.0/24 dev wlan0 proto kernel scope link metric 600\n" + routeViaISP
	if got := parseDefaultRoutes(out); len(got) != 1 {
		t.Fatalf("got %d routes, want 1", len(got))
	}
}

func TestParseDefaultRoutes_MissingMetricDefaultsToZero(t *testing.T) {
	routes := parseDefaultRoutes("default via 10.0.0.1 dev wlan0\n")
	if routes[0].Metric != 0 {
		t.Fatalf("metric = %d", routes[0].Metric)
	}
}

func TestAtoiDefault(t *testing.T) {
	if atoiDefault("600", -1) != 600 {
		t.Fatal("digits should parse")
	}
	if atoiDefault("6x0", -1) != -1 {
		t.Fatal("non-digits should fall back")
	}
	if atoiDefault("", -1) != -1 {
		t.Fatal("empty should fall back")
	}
}
