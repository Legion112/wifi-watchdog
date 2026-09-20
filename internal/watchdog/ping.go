package watchdog

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"time"
)

// ErrProbeUnavailable means the probe could not be attempted at all -- no
// privileges to open the socket, for example -- as opposed to the target being
// unreachable.
//
// The distinction is the whole point. A watchdog that treats "I could not
// check" as "the network is broken" will tear a healthy link down every time it
// runs, which is exactly how the shell version of this tool behaved when its
// hardcoded probe target became unroutable.
var ErrProbeUnavailable = errors.New("probe unavailable")

// Prober reports whether an address answers. Injected so the health check can
// be tested without raw sockets.
type Prober interface {
	Probe(target netip.Addr, iface string, count int, timeout time.Duration) error
}

// ICMPPinger sends ICMP echo requests over a raw socket. Needs root or
// CAP_NET_RAW; without it Probe returns ErrProbeUnavailable.
type ICMPPinger struct{}

const (
	icmpEchoRequest = 8
	icmpEchoReply   = 0
)

// Probe sends up to count echo requests and succeeds on the first reply.
func (p ICMPPinger) Probe(target netip.Addr, iface string, count int, timeout time.Duration) error {
	if !target.IsValid() || !target.Is4() {
		return fmt.Errorf("invalid probe target %q", target)
	}
	if count < 1 {
		count = 1
	}
	var lastErr error
	for seq := 1; seq <= count; seq++ {
		err := p.probeOnce(target, seq, timeout)
		if err == nil {
			return nil
		}
		if errors.Is(err, ErrProbeUnavailable) {
			return err
		}
		lastErr = err
	}
	return fmt.Errorf("%s did not answer %d echo request(s): %w", target, count, lastErr)
}

func (p ICMPPinger) probeOnce(target netip.Addr, seq int, timeout time.Duration) error {
	conn, err := net.Dial("ip4:icmp", target.String())
	if err != nil {
		// EPERM/EACCES here means no raw-socket privilege, not a dead network.
		if errors.Is(err, os.ErrPermission) {
			return fmt.Errorf("%w: raw ICMP socket needs root or CAP_NET_RAW: %v", ErrProbeUnavailable, err)
		}
		return fmt.Errorf("dial icmp %s: %w", target, err)
	}
	defer conn.Close()

	id := os.Getpid() & 0xffff
	req := encodeEcho(id, seq)
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return err
	}
	if _, err := conn.Write(req); err != nil {
		return fmt.Errorf("send echo to %s: %w", target, err)
	}

	buf := make([]byte, 1500)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return fmt.Errorf("no echo reply from %s: %w", target, err)
		}
		msg := stripIPHeader(buf[:n])
		if matchEchoReply(msg, id, seq) {
			return nil
		}
		// Another process's ping, or an unrelated ICMP message: keep waiting
		// until the deadline rather than reporting a false failure.
	}
}

// encodeEcho builds an ICMP echo request with a filled-in checksum.
func encodeEcho(id, seq int) []byte {
	b := make([]byte, 8)
	b[0] = icmpEchoRequest
	b[1] = 0 // code
	b[4] = byte(id >> 8)
	b[5] = byte(id)
	b[6] = byte(seq >> 8)
	b[7] = byte(seq)
	sum := checksum(b)
	b[2] = byte(sum >> 8)
	b[3] = byte(sum)
	return b
}

func matchEchoReply(msg []byte, id, seq int) bool {
	if len(msg) < 8 || msg[0] != icmpEchoReply {
		return false
	}
	gotID := int(msg[4])<<8 | int(msg[5])
	gotSeq := int(msg[6])<<8 | int(msg[7])
	return gotID == id && gotSeq == seq
}

// stripIPHeader drops a leading IPv4 header if present.
//
// Raw IPv4 sockets are inconsistent about this across platforms and kernel
// versions: some hand back the IP header, some only the payload. Sniffing the
// version nibble and using IHL is cheaper and more portable than caring which.
func stripIPHeader(b []byte) []byte {
	if len(b) < 20 || b[0]>>4 != 4 {
		return b
	}
	ihl := int(b[0]&0x0f) * 4
	if ihl < 20 || ihl > len(b) {
		return b
	}
	return b[ihl:]
}

// checksum is the standard 16-bit one's-complement sum (RFC 1071).
func checksum(b []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}
