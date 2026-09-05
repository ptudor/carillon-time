//go:build linux

package server

import (
	"net/netip"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// pktinfo4 builds the IP_PKTINFO control message the kernel delivers with a
// received IPv4 datagram.
func pktinfo4(t *testing.T, header, local [4]byte) []byte {
	t.Helper()
	info := unix.Inet4Pktinfo{Ifindex: 2, Spec_dst: local, Addr: header}
	b := make([]byte, unix.CmsgSpace(unix.SizeofInet4Pktinfo))
	h := (*unix.Cmsghdr)(unsafe.Pointer(&b[0]))
	h.Level = unix.IPPROTO_IP
	h.Type = unix.IP_PKTINFO
	h.SetLen(unix.CmsgLen(unix.SizeofInet4Pktinfo))
	copy(b[unix.CmsgLen(0):], (*(*[unix.SizeofInet4Pktinfo]byte)(unsafe.Pointer(&info)))[:])
	return b
}

// TestDestinationRejectsDirectedBroadcast covers RF5X-003. A datagram sent to
// the subnet's directed broadcast address arrives with ipi_addr set to the
// broadcast address and ipi_spec_dst to the interface address; answering it
// would make one forged datagram ask every NTP server on the segment to
// reply to the victim at once.
func TestDestinationRejectsDirectedBroadcast(t *testing.T) {
	local := [4]byte{192, 168, 1, 10}
	for _, bcast := range [][4]byte{{192, 168, 1, 255}, {10, 255, 255, 255}, {172, 19, 255, 255}} {
		dst, oob, martian := destination(pktinfo4(t, bcast, local), "udp4")
		if !martian {
			t.Fatalf("%v: not reported martian (dst %v)", netip.AddrFrom4(bcast), dst)
		}
		if oob != nil || dst.IsValid() {
			t.Fatalf("%v: a martian destination must yield no reply control message", netip.AddrFrom4(bcast))
		}
	}
}

// TestDestinationUnicastRepliesFromTheLocalAddress is the other half: an
// ordinary unicast request is answered, and the reply's source comes from
// ipi_spec_dst so a multi-homed host answers on the address it was asked on.
func TestDestinationUnicastRepliesFromTheLocalAddress(t *testing.T) {
	local := [4]byte{192, 168, 1, 10}
	dst, oob, martian := destination(pktinfo4(t, local, local), "udp4")
	if martian {
		t.Fatal("a unicast request must not be martian")
	}
	if dst != netip.AddrFrom4(local) {
		t.Fatalf("destination %v, want %v", dst, netip.AddrFrom4(local))
	}
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("reply control message: %v (%d messages)", err, len(msgs))
	}
	got := *(*unix.Inet4Pktinfo)(unsafe.Pointer(&msgs[0].Data[0]))
	if got.Spec_dst != local {
		t.Fatalf("reply source %v, want %v", netip.AddrFrom4(got.Spec_dst), netip.AddrFrom4(local))
	}
}

// TestMartianReceiveFlagsIsANoOpOnLinux documents that Linux does not report
// broadcast delivery in the flags word; destination() catches it instead.
func TestMartianReceiveFlagsIsANoOpOnLinux(t *testing.T) {
	if martianReceiveFlags(0x7fffffff) {
		t.Fatal("Linux reports no broadcast flag")
	}
}
