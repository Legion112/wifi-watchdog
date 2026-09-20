// Command wifi-watchdog keeps a NetworkManager-managed Wi-Fi link alive.
//
// It checks that NetworkManager considers the interface connected, that the
// interface carries an IPv4 default route, and that the route's nexthop answers
// an ICMP echo request. When the link is genuinely broken it reactivates the
// connection profile and, only if that is not enough, restarts NetworkManager.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Legion112/wifi-watchdog/internal/watchdog"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		fmt.Fprintf(os.Stderr, "wifi-watchdog: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, out *os.File) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: wifi-watchdog <run|check|recover> [flags]")
	}
	switch args[0] {
	case "run":
		return runDaemon(args[1:], out)
	case "check":
		return runCheck(args[1:], out)
	case "recover":
		return runRecover(args[1:], out)
	case "-h", "--help", "help":
		fmt.Fprint(out, usage)
		return nil
	default:
		return fmt.Errorf("unknown command %q (want run, check or recover)", args[0])
	}
}

const usage = `wifi-watchdog -- keep a NetworkManager Wi-Fi link alive

  run       watch the link on an interval and recover when it breaks
  check     diagnose once and exit; read-only, exit 0 only when healthy
  recover   diagnose once and recover if needed

Run 'wifi-watchdog <command> -h' for that command's flags.
`

// commonFlags are shared by every subcommand.
type commonFlags struct {
	iface       *string
	connection  *string
	gateway     *string
	pingCount   *int
	pingTimeout *time.Duration
	verbose     *bool
}

func addCommonFlags(fs *flag.FlagSet) commonFlags {
	return commonFlags{
		iface:      fs.String("iface", "wlan0", "wireless interface to watch"),
		connection: fs.String("connection", "", "NetworkManager connection profile to reactivate (default: the one active on -iface)"),
		gateway: fs.String("gateway", "", "probe this address instead of the default route's nexthop\n"+
			"(leave empty -- a hardcoded gateway goes stale when the LAN is renumbered,\n"+
			"which makes the health check fail forever and the watchdog bounce a healthy link)"),
		pingCount:   fs.Int("ping-count", watchdog.DefaultPingCount, "echo requests per probe"),
		pingTimeout: fs.Duration("ping-timeout", watchdog.DefaultPingTimeout, "per-request echo timeout"),
		verbose:     fs.Bool("verbose", false, "log healthy checks too"),
	}
}

func (c commonFlags) config() (watchdog.Config, error) {
	cfg := watchdog.Config{
		Iface:       *c.iface,
		Connection:  *c.connection,
		PingCount:   *c.pingCount,
		PingTimeout: *c.pingTimeout,
	}
	if s := strings.TrimSpace(*c.gateway); s != "" {
		a, err := netip.ParseAddr(s)
		if err != nil || !a.Is4() {
			return cfg, fmt.Errorf("invalid -gateway %q: want an IPv4 address", s)
		}
		cfg.Gateway = a
	}
	return cfg, nil
}

func (c commonFlags) logger(out *os.File) *slog.Logger {
	level := slog.LevelInfo
	if *c.verbose {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: level}))
}

func runDaemon(args []string, out *os.File) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	common := addCommonFlags(fs)
	interval := fs.Duration("interval", watchdog.DefaultInterval, "how often to check")
	threshold := fs.Int("failure-threshold", watchdog.DefaultFailureThreshold,
		"consecutive unhealthy checks required before recovering")
	cooldown := fs.Duration("cooldown", watchdog.DefaultCooldown,
		"minimum gap between recovery attempts; bounds the damage of a misdiagnosis")
	allowRestart := fs.Bool("allow-restart", true, "allow restarting NetworkManager when reactivating is not enough")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := common.config()
	if err != nil {
		return err
	}
	cfg.Interval, cfg.FailureThreshold, cfg.Cooldown, cfg.AllowRestart = *interval, *threshold, *cooldown, *allowRestart

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Deliberately no eager connection lookup here. At boot this service can
	// start before NetworkManager has associated the interface, and exiting
	// then would mean the watchdog is absent exactly when the link is down --
	// the opposite of its job. The profile name is learned on the first tick
	// that sees one.
	return watchdog.New(watchdog.ExecRunner{}, watchdog.ICMPPinger{}, common.logger(out), cfg).Run(ctx)
}

func runCheck(args []string, out *os.File) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	common := addCommonFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := common.config()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	h := watchdog.Check(ctx, watchdog.ExecRunner{}, watchdog.ICMPPinger{}, cfg.WithDefaults().CheckConfig())
	fmt.Fprintf(out, "%s\n", h)
	if h.Device.Device != "" {
		fmt.Fprintf(out, "  device    %s state=%s connection=%s\n", h.Device.Device, h.Device.State, h.Device.Connection)
	}
	if h.Route.Line != "" {
		fmt.Fprintf(out, "  route     %s\n", h.Route.Line)
	}
	if h.Gateway.IsValid() {
		fmt.Fprintf(out, "  gateway   %s\n", h.Gateway)
	}
	switch h.Verdict {
	case watchdog.Healthy:
		return nil
	case watchdog.Unknown:
		// Exit 2 so callers can tell "cannot tell" from "broken".
		os.Exit(2)
		return nil
	default:
		os.Exit(1)
		return nil
	}
}

func runRecover(args []string, out *os.File) error {
	fs := flag.NewFlagSet("recover", flag.ContinueOnError)
	common := addCommonFlags(fs)
	allowRestart := fs.Bool("allow-restart", true, "allow restarting NetworkManager when reactivating is not enough")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := common.config()
	if err != nil {
		return err
	}
	cfg.AllowRestart = *allowRestart
	// One-shot: act on the first unhealthy verdict, with no cooldown history.
	cfg.FailureThreshold, cfg.Cooldown = 1, 0

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	res := watchdog.New(watchdog.ExecRunner{}, watchdog.ICMPPinger{}, common.logger(out), cfg).Tick(ctx)
	if res.Health.Verdict != watchdog.Healthy {
		return fmt.Errorf("%s", res.Health)
	}
	return nil
}
