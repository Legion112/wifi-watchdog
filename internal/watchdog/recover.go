package watchdog

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// RecoveryOutcome describes how far the ladder had to go.
type RecoveryOutcome int

const (
	// RecoveredByActivate means `nmcli connection up` was enough.
	RecoveredByActivate RecoveryOutcome = iota
	// RecoveredByRestart means NetworkManager had to be restarted.
	RecoveredByRestart
	// RecoveryFailed means the link is still unhealthy afterwards.
	RecoveryFailed
)

func (o RecoveryOutcome) String() string {
	switch o {
	case RecoveredByActivate:
		return "recovered via nmcli connection up"
	case RecoveredByRestart:
		return "recovered after NetworkManager restart"
	default:
		return "still unhealthy after all recovery steps"
	}
}

// RecoverConfig configures the recovery ladder.
type RecoverConfig struct {
	Connection string
	Check      CheckConfig
	// ActivateTimeout is passed to nmcli -w.
	ActivateTimeout time.Duration
	// SettleAfterActivate/SettleAfterRestart are how long to wait before
	// re-checking, so the verdict is not racing the reassociation.
	SettleAfterActivate time.Duration
	SettleAfterRestart  time.Duration
	// AllowRestart gates the heavier step. With it off, recovery stops after
	// the connection-up attempt.
	AllowRestart bool
}

// sleeper is replaced in tests so the ladder does not really wait.
type sleeper func(context.Context, time.Duration)

func realSleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// Recover walks the escalation ladder: reactivate the profile, and only if that
// is not enough restart NetworkManager. It re-checks after each step so the
// gentlest sufficient action wins.
func Recover(ctx context.Context, r Runner, p Prober, log *slog.Logger, cfg RecoverConfig) (RecoveryOutcome, Health) {
	return recoverWith(ctx, r, p, log, cfg, realSleep)
}

func recoverWith(ctx context.Context, r Runner, p Prober, log *slog.Logger, cfg RecoverConfig, sleep sleeper) (RecoveryOutcome, Health) {
	log.Info("attempting recovery", "step", "nmcli connection up", "connection", cfg.Connection)
	if err := ActivateConnection(ctx, r, cfg.Connection, cfg.ActivateTimeout); err != nil {
		log.Warn("connection up failed", "error", err)
	} else {
		sleep(ctx, cfg.SettleAfterActivate)
		if h := Check(ctx, r, p, cfg.Check); h.Verdict == Healthy {
			return RecoveredByActivate, h
		}
	}

	if !cfg.AllowRestart {
		h := Check(ctx, r, p, cfg.Check)
		return RecoveryFailed, h
	}

	log.Warn("reconnect insufficient", "step", "systemctl restart NetworkManager")
	if err := RestartNetworkManager(ctx, r); err != nil {
		log.Error("NetworkManager restart failed", "error", err)
		return RecoveryFailed, Check(ctx, r, p, cfg.Check)
	}
	sleep(ctx, cfg.SettleAfterRestart)

	// After a restart NM may auto-activate; force the known profile anyway.
	if err := ActivateConnection(ctx, r, cfg.Connection, cfg.ActivateTimeout); err != nil {
		log.Warn("post-restart connection up failed", "error", err)
	}
	sleep(ctx, cfg.SettleAfterActivate)

	if h := Check(ctx, r, p, cfg.Check); h.Verdict == Healthy {
		return RecoveredByRestart, h
	}
	return RecoveryFailed, Check(ctx, r, p, cfg.Check)
}

// String for logging a health snapshot compactly.
func (h Health) String() string {
	return fmt.Sprintf("%s: %s", h.Verdict, h.Reason)
}
