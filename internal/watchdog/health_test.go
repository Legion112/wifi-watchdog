package watchdog

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// fakeProber records probes and returns a scripted result.
type fakeProber struct {
	err     error
	targets []netip.Addr
	count   int
}

func (f *fakeProber) Probe(target netip.Addr, _ string, _ int, _ time.Duration) error {
	f.targets = append(f.targets, target)
	f.count++
	return f.err
}

func healthyRunner() *RecordingRunner {
	r := deviceRunner()
	r.Outputs["ip -4 route show default dev wlan0"] = routeViaISP
	return r
}

func checkCfg() CheckConfig {
	return CheckConfig{Iface: "wlan0", PingCount: 2, PingTimeout: time.Second}
}

func TestCheck_HealthyWhenEverythingPasses(t *testing.T) {
	p := &fakeProber{}
	h := Check(context.Background(), healthyRunner(), p, checkCfg())
	if h.Verdict != Healthy {
		t.Fatalf("verdict = %s (%s)", h.Verdict, h.Reason)
	}
	if len(p.targets) != 1 || p.targets[0].String() != "192.168.8.1" {
		t.Fatalf("should probe the derived nexthop, got %v", p.targets)
	}
}

func TestCheck_UnhealthyWhenDeviceNotConnected(t *testing.T) {
	r := healthyRunner()
	r.Outputs["nmcli -t -f DEVICE,STATE,CONNECTION device status"] = "wlan0:disconnected:"
	h := Check(context.Background(), r, &fakeProber{}, checkCfg())
	if h.Verdict != Unhealthy {
		t.Fatalf("verdict = %s", h.Verdict)
	}
}

func TestCheck_UnhealthyWhenNoDefaultRoute(t *testing.T) {
	r := healthyRunner()
	r.Outputs["ip -4 route show default dev wlan0"] = "\n"
	h := Check(context.Background(), r, &fakeProber{}, checkCfg())
	if h.Verdict != Unhealthy || !strings.Contains(h.Reason, "default route") {
		t.Fatalf("got %s: %s", h.Verdict, h.Reason)
	}
}

func TestCheck_UnhealthyWhenGatewaySilent(t *testing.T) {
	h := Check(context.Background(), healthyRunner(), &fakeProber{err: errors.New("timeout")}, checkCfg())
	if h.Verdict != Unhealthy || !strings.Contains(h.Reason, "unreachable") {
		t.Fatalf("got %s: %s", h.Verdict, h.Reason)
	}
}

// The central guard. "I could not run the probe" must be Unknown, never
// Unhealthy: a privilege problem that reads as a broken network makes the
// watchdog restart the network every single time it runs.
func TestCheck_ProbeUnavailableIsUnknownNotUnhealthy(t *testing.T) {
	p := &fakeProber{err: fmt.Errorf("%w: no CAP_NET_RAW", ErrProbeUnavailable)}
	h := Check(context.Background(), healthyRunner(), p, checkCfg())
	if h.Verdict != Unknown {
		t.Fatalf("verdict = %s, want unknown so no recovery is triggered", h.Verdict)
	}
}

// Likewise, not being able to ask NetworkManager is inconclusive.
func TestCheck_DeviceQueryFailureIsUnknown(t *testing.T) {
	r := healthyRunner()
	r.FailOn = "nmcli"
	h := Check(context.Background(), r, &fakeProber{}, checkCfg())
	if h.Verdict != Unknown {
		t.Fatalf("verdict = %s, want unknown", h.Verdict)
	}
}

func TestCheck_GatewayOverrideIsProbedInsteadOfDerived(t *testing.T) {
	cfg := checkCfg()
	cfg.Gateway = netip.MustParseAddr("10.0.0.1")
	p := &fakeProber{}
	Check(context.Background(), healthyRunner(), p, cfg)
	if len(p.targets) != 1 || p.targets[0].String() != "10.0.0.1" {
		t.Fatalf("override ignored, probed %v", p.targets)
	}
}

// An on-link default route has no nexthop; inventing one to probe would be the
// same mistake as hardcoding a gateway.
func TestCheck_OnLinkDefaultRouteIsHealthyWithoutProbing(t *testing.T) {
	r := healthyRunner()
	r.Outputs["ip -4 route show default dev wlan0"] = "default dev wlan0 scope link metric 600\n"
	p := &fakeProber{}
	h := Check(context.Background(), r, p, checkCfg())
	if h.Verdict != Healthy {
		t.Fatalf("got %s: %s", h.Verdict, h.Reason)
	}
	if p.count != 0 {
		t.Fatal("must not probe when there is no nexthop")
	}
}

func TestVerdict_String(t *testing.T) {
	for v, want := range map[Verdict]string{Healthy: "healthy", Unhealthy: "unhealthy", Unknown: "unknown"} {
		if v.String() != want {
			t.Errorf("%d = %q, want %q", v, v.String(), want)
		}
	}
}
