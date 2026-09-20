package watchdog

import (
	"context"
	"log/slog"
	"net/netip"
	"time"
)

// Defaults chosen to be conservative: the tool's failure mode of last resort
// should be "did nothing", never "bounced a working link".
const (
	DefaultInterval            = time.Minute
	DefaultPingCount           = 2
	DefaultPingTimeout         = 2 * time.Second
	DefaultFailureThreshold    = 2
	DefaultCooldown            = 5 * time.Minute
	DefaultActivateTimeout     = 30 * time.Second
	DefaultSettleAfterActivate = 3 * time.Second
	DefaultSettleAfterRestart  = 5 * time.Second
)

// Config is the watchdog's full configuration.
type Config struct {
	Iface      string
	Connection string
	// Gateway overrides the derived default-route nexthop. Leave zero to derive.
	Gateway netip.Addr

	Interval    time.Duration
	PingCount   int
	PingTimeout time.Duration

	// FailureThreshold is how many consecutive unhealthy checks must occur
	// before recovery runs. Anything above 1 stops a single dropped packet
	// from tearing the link down.
	FailureThreshold int
	// Cooldown is the minimum gap between recovery attempts. It bounds the
	// damage of a misdiagnosis: at worst the link is bounced once per
	// cooldown, not once per interval.
	Cooldown time.Duration

	AllowRestart        bool
	ActivateTimeout     time.Duration
	SettleAfterActivate time.Duration
	SettleAfterRestart  time.Duration
}

// WithDefaults fills in unset fields.
func (c Config) WithDefaults() Config {
	if c.Interval <= 0 {
		c.Interval = DefaultInterval
	}
	if c.PingCount <= 0 {
		c.PingCount = DefaultPingCount
	}
	if c.PingTimeout <= 0 {
		c.PingTimeout = DefaultPingTimeout
	}
	if c.FailureThreshold <= 0 {
		c.FailureThreshold = DefaultFailureThreshold
	}
	if c.Cooldown < 0 {
		c.Cooldown = DefaultCooldown
	}
	if c.ActivateTimeout <= 0 {
		c.ActivateTimeout = DefaultActivateTimeout
	}
	if c.SettleAfterActivate <= 0 {
		c.SettleAfterActivate = DefaultSettleAfterActivate
	}
	if c.SettleAfterRestart <= 0 {
		c.SettleAfterRestart = DefaultSettleAfterRestart
	}
	return c
}

// CheckConfig exposes the check settings derived from this config.
func (c Config) CheckConfig() CheckConfig { return c.checkConfig() }

func (c Config) checkConfig() CheckConfig {
	return CheckConfig{
		Iface:       c.Iface,
		Gateway:     c.Gateway,
		PingCount:   c.PingCount,
		PingTimeout: c.PingTimeout,
	}
}

func (c Config) recoverConfig() RecoverConfig {
	return RecoverConfig{
		Connection:          c.Connection,
		Check:               c.checkConfig(),
		ActivateTimeout:     c.ActivateTimeout,
		SettleAfterActivate: c.SettleAfterActivate,
		SettleAfterRestart:  c.SettleAfterRestart,
		AllowRestart:        c.AllowRestart,
	}
}

// Watchdog holds the debounce and cooldown state between checks.
//
// It is a long-running daemon rather than a timer-driven one-shot on purpose:
// consecutive-failure and cooldown state stay in memory, and there is no
// systemd timer schedule to get wedged. (A timer using OnBootSec plus
// OnUnitActiveSec silently stops firing for the rest of the boot if it is
// re-enabled after the OnBootSec window has already passed.)
type Watchdog struct {
	cfg Config
	r   Runner
	p   Prober
	log *slog.Logger

	now   func() time.Time
	sleep sleeper

	consecutiveFailures int
	lastRecovery        time.Time
}

// New builds a Watchdog.
func New(r Runner, p Prober, log *slog.Logger, cfg Config) *Watchdog {
	return &Watchdog{
		cfg:   cfg.WithDefaults(),
		r:     r,
		p:     p,
		log:   log,
		now:   time.Now,
		sleep: realSleep,
	}
}

// TickResult reports what one iteration concluded and did.
type TickResult struct {
	Health     Health
	Acted      bool
	Outcome    RecoveryOutcome
	Suppressed string // non-empty when recovery was warranted but withheld
}

// Tick runs one check and, if warranted, one recovery.
func (w *Watchdog) Tick(ctx context.Context) TickResult {
	h := Check(ctx, w.r, w.p, w.cfg.checkConfig())
	res := TickResult{Health: h}

	switch h.Verdict {
	case Healthy:
		if w.consecutiveFailures > 0 {
			w.log.Info("link healthy again", "after_failures", w.consecutiveFailures, "reason", h.Reason)
		} else {
			w.log.Debug("link healthy", "reason", h.Reason)
		}
		w.consecutiveFailures = 0
		return res

	case Unknown:
		// Inconclusive: do not count it as a failure and do not act. This is
		// the guard that keeps a privilege or tooling problem from being
		// "fixed" by repeatedly restarting the network.
		w.log.Warn("health indeterminate, taking no action", "reason", h.Reason)
		return res
	}

	w.consecutiveFailures++
	w.log.Warn("link unhealthy", "reason", h.Reason,
		"consecutive", w.consecutiveFailures, "threshold", w.cfg.FailureThreshold)

	if w.consecutiveFailures < w.cfg.FailureThreshold {
		res.Suppressed = "below failure threshold"
		return res
	}
	if !w.lastRecovery.IsZero() && w.cfg.Cooldown > 0 {
		if since := w.now().Sub(w.lastRecovery); since < w.cfg.Cooldown {
			res.Suppressed = "in cooldown"
			w.log.Warn("recovery suppressed by cooldown",
				"since_last", since.Round(time.Second), "cooldown", w.cfg.Cooldown)
			return res
		}
	}

	w.lastRecovery = w.now()
	outcome, after := recoverWith(ctx, w.r, w.p, w.log, w.cfg.recoverConfig(), w.sleep)
	res.Acted, res.Outcome, res.Health = true, outcome, after
	if outcome == RecoveryFailed {
		w.log.Error("recovery failed", "outcome", outcome.String(), "reason", after.Reason)
	} else {
		w.log.Info("recovery succeeded", "outcome", outcome.String())
		w.consecutiveFailures = 0
	}
	return res
}

// Run checks on Interval until ctx is cancelled.
func (w *Watchdog) Run(ctx context.Context) error {
	w.log.Info("watchdog started",
		"iface", w.cfg.Iface, "connection", w.cfg.Connection,
		"interval", w.cfg.Interval, "threshold", w.cfg.FailureThreshold,
		"cooldown", w.cfg.Cooldown, "allow_restart", w.cfg.AllowRestart)

	ticker := time.NewTicker(w.cfg.Interval)
	defer ticker.Stop()
	w.Tick(ctx)
	for {
		select {
		case <-ctx.Done():
			w.log.Info("watchdog stopping")
			return ctx.Err()
		case <-ticker.C:
			w.Tick(ctx)
		}
	}
}
