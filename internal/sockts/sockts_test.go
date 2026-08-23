package sockts

import (
	"net"
	"runtime"
	"testing"
	"time"
)

func TestParseEmpty(t *testing.T) {
	if _, ok := Parse(nil); ok {
		t.Fatal("empty oob must not yield a timestamp")
	}
	if _, ok := Parse(make([]byte, OOBSize)); ok {
		t.Fatal("zeroed oob must not yield a timestamp")
	}
}

// TestLoopback enables timestamps on a loopback socket and sends a datagram
// to itself. On Linux and FreeBSD the kernel must attach a timestamp close
// to now; elsewhere Enable is a no-op and Parse finds nothing.
func TestLoopback(t *testing.T) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer conn.Close()
	rc, err := conn.SyscallConn()
	if err != nil {
		t.Fatalf("SyscallConn: %v", err)
	}
	if err := Enable(rc); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	self := conn.LocalAddr().(*net.UDPAddr).AddrPort()
	before := time.Now()
	if _, err := conn.WriteToUDPAddrPort([]byte("ping"), self); err != nil {
		t.Fatalf("send: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	oob := make([]byte, OOBSize)
	_, oobn, _, _, err := conn.ReadMsgUDPAddrPort(buf, oob)
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	after := time.Now()
	ts, ok := Parse(oob[:oobn])
	switch runtime.GOOS {
	case "linux", "freebsd":
		if !ok {
			t.Fatal("kernel timestamp missing")
		}
		if ts.Before(before.Add(-time.Second)) || ts.After(after.Add(time.Second)) {
			t.Fatalf("timestamp %v not between %v and %v", ts, before, after)
		}
	default:
		if ok {
			t.Fatalf("unexpected timestamp %v on %s", ts, runtime.GOOS)
		}
	}
}
