package watchdog

import (
	"errors"
	"net/netip"
	"testing"
	"time"
)

func TestChecksum_KnownVector(t *testing.T) {
	// An echo request with a zeroed checksum field; verifying the sum over the
	// completed message is zero is the standard self-check.
	msg := encodeEcho(0x1234, 1)
	if got := checksum(msg); got != 0 {
		t.Fatalf("checksum over a summed message = %#04x, want 0", got)
	}
}

func TestChecksum_OddLengthPayload(t *testing.T) {
	// Must not panic and must fold the trailing byte in.
	if checksum([]byte{0x01, 0x02, 0x03}) == 0 {
		t.Fatal("expected a non-zero checksum")
	}
}

func TestEncodeEcho_HeaderFields(t *testing.T) {
	b := encodeEcho(0xbeef, 0x0102)
	if b[0] != icmpEchoRequest || b[1] != 0 {
		t.Fatalf("type/code = %d/%d", b[0], b[1])
	}
	if int(b[4])<<8|int(b[5]) != 0xbeef {
		t.Fatal("id not encoded big-endian")
	}
	if int(b[6])<<8|int(b[7]) != 0x0102 {
		t.Fatal("seq not encoded big-endian")
	}
}

func TestMatchEchoReply_AcceptsMatchingIDAndSeq(t *testing.T) {
	reply := []byte{icmpEchoReply, 0, 0, 0, 0xbe, 0xef, 0x01, 0x02}
	if !matchEchoReply(reply, 0xbeef, 0x0102) {
		t.Fatal("should match")
	}
}

// Another process's ping must not be mistaken for ours.
func TestMatchEchoReply_RejectsForeignID(t *testing.T) {
	reply := []byte{icmpEchoReply, 0, 0, 0, 0x00, 0x01, 0x01, 0x02}
	if matchEchoReply(reply, 0xbeef, 0x0102) {
		t.Fatal("must not match a different identifier")
	}
}

func TestMatchEchoReply_RejectsWrongSequence(t *testing.T) {
	reply := []byte{icmpEchoReply, 0, 0, 0, 0xbe, 0xef, 0x09, 0x09}
	if matchEchoReply(reply, 0xbeef, 0x0102) {
		t.Fatal("must not match a different sequence")
	}
}

func TestMatchEchoReply_RejectsNonEchoReplyType(t *testing.T) {
	// e.g. a destination-unreachable message.
	if matchEchoReply([]byte{3, 1, 0, 0, 0xbe, 0xef, 0x01, 0x02}, 0xbeef, 0x0102) {
		t.Fatal("only echo replies count")
	}
}

func TestMatchEchoReply_RejectsShortMessage(t *testing.T) {
	if matchEchoReply([]byte{icmpEchoReply, 0, 0}, 1, 1) {
		t.Fatal("a truncated message must not match")
	}
}

// Raw IPv4 sockets differ across platforms on whether the IP header is handed
// back, so the reader sniffs and strips it.
func TestStripIPHeader_RemovesMinimalHeader(t *testing.T) {
	pkt := make([]byte, 20+8)
	pkt[0] = 0x45 // version 4, IHL 5 -> 20 bytes
	pkt[20] = icmpEchoReply
	if got := stripIPHeader(pkt); len(got) != 8 || got[0] != icmpEchoReply {
		t.Fatalf("got %d bytes starting %#02x", len(got), got[0])
	}
}

func TestStripIPHeader_HonoursIHLWithOptions(t *testing.T) {
	pkt := make([]byte, 24+8)
	pkt[0] = 0x46 // IHL 6 -> 24 bytes
	pkt[24] = icmpEchoReply
	if got := stripIPHeader(pkt); len(got) != 8 || got[0] != icmpEchoReply {
		t.Fatalf("got %d bytes", len(got))
	}
}

func TestStripIPHeader_LeavesBareICMPAlone(t *testing.T) {
	msg := []byte{icmpEchoReply, 0, 0, 0, 0, 1, 0, 1}
	if got := stripIPHeader(msg); len(got) != len(msg) {
		t.Fatalf("bare ICMP should be untouched, got %d bytes", len(got))
	}
}

func TestStripIPHeader_IgnoresImplausibleIHL(t *testing.T) {
	pkt := make([]byte, 24)
	pkt[0] = 0x4f // IHL 15 -> 60 bytes, longer than the buffer
	if got := stripIPHeader(pkt); len(got) != len(pkt) {
		t.Fatal("must not slice past the buffer")
	}
}

func TestICMPPinger_RejectsInvalidTarget(t *testing.T) {
	if err := (ICMPPinger{}).Probe(netip.Addr{}, "wlan0", 1, time.Second); err == nil {
		t.Fatal("want an error for an invalid target")
	}
}

func TestICMPPinger_RejectsIPv6Target(t *testing.T) {
	err := (ICMPPinger{}).Probe(netip.MustParseAddr("2001:db8::1"), "wlan0", 1, time.Second)
	if err == nil {
		t.Fatal("want an error for a v6 target")
	}
}

func TestErrProbeUnavailable_IsIdentifiable(t *testing.T) {
	wrapped := errors.New("x")
	if errors.Is(wrapped, ErrProbeUnavailable) {
		t.Fatal("unrelated errors must not match")
	}
	if !errors.Is(errWrap(ErrProbeUnavailable), ErrProbeUnavailable) {
		t.Fatal("wrapped sentinel must be identifiable so callers can treat it as inconclusive")
	}
}

func errWrap(err error) error { return errors.Join(err, errors.New("context")) }
