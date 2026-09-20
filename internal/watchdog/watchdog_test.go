package watchdog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// newTestWatchdog wires a watchdog with a controllable clock and no real sleeps.
func newTestWatchdog(r Runner, p Prober, cfg Config) (*Watchdog, *time.Time) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	w := New(r, p, quietLog(), cfg)
	w.now = func() time.Time { return now }
	w.sleep = func(context.Context, time.Duration) {}
	return w, &now
}

func brokenRunner() *RecordingRunner {
	r := deviceRunner()
	r.Outputs["nmcli -t -f DEVICE,STATE,CONNECTION device status"] = "wlan0:disconnected:"
	r.Outputs["ip -4 route show default dev wlan0"] = routeViaISP
	return r
}

// healingRunner reports an unhealthy device until `nmcli connection up` is
// issued, then a healthy one -- modelling a link that reactivation fixes.
type healingRunner struct {
	*RecordingRunner
	healed bool
}

func newHealingRunner() *healingRunner {
	h := &healingRunner{RecordingRunner: NewRecordingRunner()}
	h.Outputs["ip -4 route show default dev wlan0"] = routeViaISP
	return h
}

func (h *healingRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	key := name + " " + strings.Join(args, " ")
	if strings.Contains(key, "connection up") {
		h.healed = true
	}
	if strings.Contains(key, "device status") {
		h.Calls = append(h.Calls, key)
		if h.healed {
			return fixtureDeviceStatus, nil
		}
		return "wlan0:disconnected:", nil
	}
	return h.RecordingRunner.Run(ctx, name, args...)
}

func baseConfig() Config {
	return Config{Iface: "wlan0", Connection: "MTS_GPON_2F34", FailureThreshold: 1, AllowRestart: true}
}

func TestTick_HealthyDoesNothing(t *testing.T) {
	r := healthyRunner()
	w, _ := newTestWatchdog(r, &fakeProber{}, baseConfig())
	res := w.Tick(context.Background())
	if res.Acted {
		t.Fatal("a healthy link must not be touched")
	}
	if strings.Contains(r.CallLog(), "connection up") || strings.Contains(r.CallLog(), "restart") {
		t.Fatalf("no recovery commands expected:\n%s", r.CallLog())
	}
}

// The exact failure mode of the shell version: a probe that cannot run must
// never cause the link to be bounced.
func TestTick_ProbeUnavailableNeverRecovers(t *testing.T) {
	r := healthyRunner()
	p := &fakeProber{err: fmt.Errorf("%w: no privileges", ErrProbeUnavailable)}
	w, _ := newTestWatchdog(r, p, baseConfig())
	for i := 0; i < 5; i++ {
		if res := w.Tick(context.Background()); res.Acted {
			t.Fatal("must not act on an indeterminate verdict")
		}
	}
	if strings.Contains(r.CallLog(), "connection up") || strings.Contains(r.CallLog(), "restart") {
		t.Fatalf("no recovery expected across repeated unknown verdicts:\n%s", r.CallLog())
	}
	if w.consecutiveFailures != 0 {
		t.Fatalf("unknown verdicts must not accrue failures, got %d", w.consecutiveFailures)
	}
}

func TestTick_RecoversWhenGenuinelyBroken(t *testing.T) {
	r := brokenRunner()
	w, _ := newTestWatchdog(r, &fakeProber{}, baseConfig())
	res := w.Tick(context.Background())
	if !res.Acted {
		t.Fatal("expected recovery")
	}
	if !strings.Contains(r.CallLog(), "nmcli -w 30 connection up MTS_GPON_2F34") {
		t.Fatalf("expected a reactivation:\n%s", r.CallLog())
	}
}

// A single dropped probe should not be enough to bounce the link.
func TestTick_BelowFailureThresholdDoesNotAct(t *testing.T) {
	cfg := baseConfig()
	cfg.FailureThreshold = 3
	r := brokenRunner()
	w, _ := newTestWatchdog(r, &fakeProber{}, cfg)

	for i := 1; i <= 2; i++ {
		res := w.Tick(context.Background())
		if res.Acted {
			t.Fatalf("acted on failure %d of 3", i)
		}
		if res.Suppressed != "below failure threshold" {
			t.Fatalf("suppression reason = %q", res.Suppressed)
		}
	}
	if res := w.Tick(context.Background()); !res.Acted {
		t.Fatal("should act once the threshold is reached")
	}
}

func TestTick_RecoveryResetsFailureCounter(t *testing.T) {
	cfg := baseConfig()
	cfg.FailureThreshold = 2
	r := newHealingRunner()
	w, _ := newTestWatchdog(r, &fakeProber{}, cfg)

	if res := w.Tick(context.Background()); res.Acted {
		t.Fatal("first failure is below the threshold")
	}
	res := w.Tick(context.Background())
	if !res.Acted || res.Outcome != RecoveredByActivate {
		t.Fatalf("acted=%v outcome=%v", res.Acted, res.Outcome)
	}
	if w.consecutiveFailures != 0 {
		t.Fatalf("counter should reset after a successful recovery, got %d", w.consecutiveFailures)
	}
}

func TestTick_HealthyResetsFailureCounter(t *testing.T) {
	cfg := baseConfig()
	cfg.FailureThreshold = 5
	r := brokenRunner()
	w, _ := newTestWatchdog(r, &fakeProber{}, cfg)
	w.Tick(context.Background())
	w.Tick(context.Background())
	r.Outputs["nmcli -t -f DEVICE,STATE,CONNECTION device status"] = fixtureDeviceStatus
	w.Tick(context.Background())
	if w.consecutiveFailures != 0 {
		t.Fatalf("got %d", w.consecutiveFailures)
	}
}

// Cooldown bounds the damage of a misdiagnosis: at worst one bounce per
// cooldown, rather than one per interval forever.
func TestTick_CooldownSuppressesRepeatedRecovery(t *testing.T) {
	cfg := baseConfig()
	cfg.Cooldown = 5 * time.Minute
	r := brokenRunner()
	w, now := newTestWatchdog(r, &fakeProber{}, cfg)

	if res := w.Tick(context.Background()); !res.Acted {
		t.Fatal("first tick should act")
	}
	callsAfterFirst := len(r.Calls)

	*now = now.Add(time.Minute)
	res := w.Tick(context.Background())
	if res.Acted {
		t.Fatal("second attempt one minute later must be suppressed")
	}
	if res.Suppressed != "in cooldown" {
		t.Fatalf("suppression reason = %q", res.Suppressed)
	}

	*now = now.Add(5 * time.Minute)
	if res := w.Tick(context.Background()); !res.Acted {
		t.Fatal("should act again once the cooldown has elapsed")
	}
	if len(r.Calls) <= callsAfterFirst {
		t.Fatal("expected further recovery commands after the cooldown")
	}
}

func TestTick_ZeroCooldownAllowsConsecutiveRecovery(t *testing.T) {
	cfg := baseConfig()
	cfg.Cooldown = 0
	w, _ := newTestWatchdog(brokenRunner(), &fakeProber{}, cfg)
	w.Tick(context.Background())
	if res := w.Tick(context.Background()); !res.Acted {
		t.Fatal("with no cooldown the second tick should also act")
	}
}

func TestRecover_EscalatesToRestartOnlyWhenNeeded(t *testing.T) {
	r := brokenRunner()
	w, _ := newTestWatchdog(r, &fakeProber{}, baseConfig())
	res := w.Tick(context.Background())
	if res.Outcome != RecoveryFailed {
		t.Fatalf("outcome = %v", res.Outcome)
	}
	log := r.CallLog()
	upIdx := strings.Index(log, "connection up")
	restartIdx := strings.Index(log, "systemctl restart NetworkManager")
	if upIdx < 0 || restartIdx < 0 {
		t.Fatalf("expected both steps:\n%s", log)
	}
	if upIdx > restartIdx {
		t.Fatal("the gentler reactivation must be tried before restarting NetworkManager")
	}
}

// The gentlest sufficient action must win: if reactivating fixes the link,
// NetworkManager must not be restarted.
func TestRecover_StopsAtActivateWhenThatFixesIt(t *testing.T) {
	r := newHealingRunner()
	w, _ := newTestWatchdog(r, &fakeProber{}, baseConfig())
	res := w.Tick(context.Background())
	if !res.Acted || res.Outcome != RecoveredByActivate {
		t.Fatalf("acted=%v outcome=%v", res.Acted, res.Outcome)
	}
	if strings.Contains(r.CallLog(), "systemctl restart") {
		t.Fatalf("must not escalate when reactivation was enough:\n%s", r.CallLog())
	}
}

func TestRecover_AllowRestartFalseNeverRestarts(t *testing.T) {
	cfg := baseConfig()
	cfg.AllowRestart = false
	r := brokenRunner()
	w, _ := newTestWatchdog(r, &fakeProber{}, cfg)
	w.Tick(context.Background())
	if strings.Contains(r.CallLog(), "systemctl restart") {
		t.Fatalf("-allow-restart=false must not restart NetworkManager:\n%s", r.CallLog())
	}
}

func TestRecover_ActivateFailureStillEscalates(t *testing.T) {
	r := brokenRunner()
	r.Errors["nmcli -w 30 connection up MTS_GPON_2F34"] = errors.New("boom")
	w, _ := newTestWatchdog(r, &fakeProber{}, baseConfig())
	w.Tick(context.Background())
	if !strings.Contains(r.CallLog(), "systemctl restart NetworkManager") {
		t.Fatalf("a failed reactivation should escalate:\n%s", r.CallLog())
	}
}

func TestRecover_RestartFailureIsReportedNotPanicking(t *testing.T) {
	r := brokenRunner()
	r.Errors["systemctl restart NetworkManager"] = errors.New("dbus gone")
	w, _ := newTestWatchdog(r, &fakeProber{}, baseConfig())
	if res := w.Tick(context.Background()); res.Outcome != RecoveryFailed {
		t.Fatalf("outcome = %v", res.Outcome)
	}
}

func TestConfig_WithDefaultsFillsUnsetFields(t *testing.T) {
	c := Config{}.WithDefaults()
	if c.Interval != DefaultInterval || c.PingCount != DefaultPingCount ||
		c.FailureThreshold != DefaultFailureThreshold || c.ActivateTimeout != DefaultActivateTimeout {
		t.Fatalf("got %+v", c)
	}
}

func TestConfig_WithDefaultsKeepsExplicitZeroCooldown(t *testing.T) {
	// Zero is a meaningful choice ("no cooldown"); only a negative value is
	// treated as unset.
	if c := (Config{Cooldown: 0}).WithDefaults(); c.Cooldown != 0 {
		t.Fatalf("cooldown = %v, want 0 preserved", c.Cooldown)
	}
	if c := (Config{Cooldown: -1}).WithDefaults(); c.Cooldown != DefaultCooldown {
		t.Fatalf("cooldown = %v, want the default", c.Cooldown)
	}
}

func TestRun_StopsOnContextCancel(t *testing.T) {
	cfg := baseConfig()
	cfg.Interval = time.Hour
	w, _ := newTestWatchdog(healthyRunner(), &fakeProber{}, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := w.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestRecoveryOutcome_String(t *testing.T) {
	for o, want := range map[RecoveryOutcome]string{
		RecoveredByActivate: "recovered via nmcli connection up",
		RecoveredByRestart:  "recovered after NetworkManager restart",
		RecoveryFailed:      "still unhealthy after all recovery steps",
	} {
		if o.String() != want {
			t.Errorf("%d = %q", o, o.String())
		}
	}
}
