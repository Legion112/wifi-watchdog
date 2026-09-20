package watchdog

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Captured from `nmcli -t -f DEVICE,STATE,CONNECTION device status` on a
// Debian 13 host with a Wi-Fi link and a wired lifeline.
const fixtureDeviceStatus = `wlan0:connected:MTS_GPON_2F34
eth0:connected:Wired connection 1
lo:connected (externally):lo
p2p-dev-wlan0:disconnected:`

func deviceRunner() *RecordingRunner {
	r := NewRecordingRunner()
	r.Outputs["nmcli -t -f DEVICE,STATE,CONNECTION device status"] = fixtureDeviceStatus
	return r
}

func TestQueryDevice_Connected(t *testing.T) {
	d, err := QueryDevice(context.Background(), deviceRunner(), "wlan0")
	if err != nil {
		t.Fatalf("QueryDevice: %v", err)
	}
	if !d.Connected() || d.Connection != "MTS_GPON_2F34" {
		t.Fatalf("got %+v", d)
	}
}

func TestQueryDevice_DisconnectedIsNotConnected(t *testing.T) {
	r := deviceRunner()
	r.Outputs["nmcli -t -f DEVICE,STATE,CONNECTION device status"] = "wlan0:disconnected:"
	d, err := QueryDevice(context.Background(), r, "wlan0")
	if err != nil {
		t.Fatalf("QueryDevice: %v", err)
	}
	if d.Connected() {
		t.Fatal("disconnected must not report connected")
	}
}

// "connected (externally)" is not full management; only exact "connected" is.
func TestQueryDevice_ExternallyConnectedIsNotConnected(t *testing.T) {
	d, err := QueryDevice(context.Background(), deviceRunner(), "lo")
	if err != nil {
		t.Fatalf("QueryDevice: %v", err)
	}
	if d.Connected() {
		t.Fatalf("state %q must not count as connected", d.State)
	}
}

func TestQueryDevice_MissingInterfaceErrors(t *testing.T) {
	_, err := QueryDevice(context.Background(), deviceRunner(), "wlan9")
	if err == nil || !strings.Contains(err.Error(), "not present") {
		t.Fatalf("err = %v", err)
	}
}

func TestQueryDevice_CommandFailureErrors(t *testing.T) {
	r := deviceRunner()
	r.FailOn = "nmcli"
	if _, err := QueryDevice(context.Background(), r, "wlan0"); err == nil {
		t.Fatal("want error when nmcli fails")
	}
}

// A connection name may contain a colon, which nmcli escapes.
func TestQueryDevice_EscapedColonInConnectionName(t *testing.T) {
	r := NewRecordingRunner()
	r.Outputs["nmcli -t -f DEVICE,STATE,CONNECTION device status"] = `wlan0:connected:Home\:Net`
	d, err := QueryDevice(context.Background(), r, "wlan0")
	if err != nil {
		t.Fatalf("QueryDevice: %v", err)
	}
	if d.Connection != "Home:Net" {
		t.Fatalf("connection = %q", d.Connection)
	}
}

func TestSplitTerse_EscapedBackslash(t *testing.T) {
	got := splitTerse(`a\\b:c`)
	if len(got) != 2 || got[0] != `a\b` || got[1] != "c" {
		t.Fatalf("got %q", got)
	}
}

func TestActivateConnection_PassesWaitTimeout(t *testing.T) {
	r := NewRecordingRunner()
	if err := ActivateConnection(context.Background(), r, "MTS_GPON_2F34", 30*time.Second); err != nil {
		t.Fatalf("ActivateConnection: %v", err)
	}
	want := "nmcli -w 30 connection up MTS_GPON_2F34"
	if r.CallLog() != want {
		t.Fatalf("call = %q, want %q", r.CallLog(), want)
	}
}

func TestActivateConnection_ClampsSubSecondTimeout(t *testing.T) {
	r := NewRecordingRunner()
	_ = ActivateConnection(context.Background(), r, "c", 10*time.Millisecond)
	if !strings.Contains(r.CallLog(), "-w 1 ") {
		t.Fatalf("want a floor of 1s, got %q", r.CallLog())
	}
}

func TestRestartNetworkManager(t *testing.T) {
	r := NewRecordingRunner()
	if err := RestartNetworkManager(context.Background(), r); err != nil {
		t.Fatalf("RestartNetworkManager: %v", err)
	}
	if r.CallLog() != "systemctl restart NetworkManager" {
		t.Fatalf("call = %q", r.CallLog())
	}
}
