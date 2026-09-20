package watchdog

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// DeviceState is what NetworkManager reports for an interface.
type DeviceState struct {
	Device     string
	State      string // e.g. "connected", "disconnected", "unavailable"
	Connection string
}

// Connected reports whether the device is fully connected.
func (d DeviceState) Connected() bool { return d.State == "connected" }

// QueryDevice reads NetworkManager's view of iface.
//
// Output is the terse form "DEVICE:STATE:CONNECTION"; a missing interface
// yields an error rather than an empty state, so "NetworkManager does not know
// this interface" is never mistaken for "the link is down".
func QueryDevice(ctx context.Context, r Runner, iface string) (DeviceState, error) {
	out, err := r.Run(ctx, "nmcli", "-t", "-f", "DEVICE,STATE,CONNECTION", "device", "status")
	if err != nil {
		return DeviceState{}, fmt.Errorf("nmcli device status: %w", err)
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := splitTerse(line)
		if len(f) < 2 || f[0] != iface {
			continue
		}
		d := DeviceState{Device: f[0], State: f[1]}
		if len(f) > 2 {
			d.Connection = f[2]
		}
		return d, nil
	}
	return DeviceState{}, fmt.Errorf("interface %q not present in nmcli device status", iface)
}

// splitTerse splits an `nmcli -t` line on unescaped colons, honouring \: and \\.
// Connection names may legitimately contain a colon, and nmcli escapes it.
func splitTerse(line string) []string {
	var out []string
	var cur strings.Builder
	esc := false
	for _, ch := range line {
		switch {
		case esc:
			cur.WriteRune(ch)
			esc = false
		case ch == '\\':
			esc = true
		case ch == ':':
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(ch)
		}
	}
	return append(out, cur.String())
}

// ActivateConnection brings a connection profile up, waiting up to timeout.
func ActivateConnection(ctx context.Context, r Runner, conn string, timeout time.Duration) error {
	secs := int(timeout.Seconds())
	if secs < 1 {
		secs = 1
	}
	if _, err := r.Run(ctx, "nmcli", "-w", itoa(secs), "connection", "up", conn); err != nil {
		return fmt.Errorf("nmcli connection up %q: %w", conn, err)
	}
	return nil
}

// RestartNetworkManager is the heavier recovery step.
func RestartNetworkManager(ctx context.Context, r Runner) error {
	if _, err := r.Run(ctx, "systemctl", "restart", "NetworkManager"); err != nil {
		return fmt.Errorf("restart NetworkManager: %w", err)
	}
	return nil
}

func itoa(n int) string { return fmt.Sprintf("%d", n) }
