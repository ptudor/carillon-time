//go:build linux

package serial

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestNPPSKeepsDeliveringSerialData is the evidence behind letting one tty
// carry a GPS receiver's NMEA stream and its DCD pulse at the same time.
//
// carillon used to refuse that arrangement on Linux, believing N_PPS replaced
// normal tty input. It does not: pps_ldisc is registered through
// n_tty_inherit_ops() and its open handler chains to n_tty_open(), so the
// discipline is N_TTY plus a dcd_change hook. This test asserts exactly that
// property against the running kernel — bytes written before the attach are
// discarded with the old discipline's buffer, bytes written after it arrive
// normally — so a future reader does not have to take the claim on trust.
//
// It opens no hardware, reads no clock and needs no privilege: the tty is a
// pseudo-terminal this test creates and closes, and attaching a discipline to
// it needs only write access to a descriptor the test owns. It is therefore
// not gated behind hwtest. Where the kernel will not put N_PPS on a pty at
// all — the module is absent, or the driver refuses — the test skips rather
// than failing, so it can never report a problem it did not actually observe.
func TestNPPSKeepsDeliveringSerialData(t *testing.T) {
	master, slavePath := openPTY(t)

	port, err := OpenNMEA(slavePath, 9600)
	if err != nil {
		t.Skipf("cannot open %s in raw mode: %v", slavePath, err)
	}
	t.Cleanup(func() { _ = port.Close() })

	// Baseline: the pty carries bytes under the default discipline, so a
	// later failure to read cannot be blamed on the harness.
	const before = "$GPRMC,before,A*00\r\n"
	writeAll(t, master, before)
	if got := readChunk(t, port); got != before {
		t.Fatalf("under N_TTY the pty delivered %q, want %q", got, before)
	}

	if err := unix.IoctlSetPointerInt(port.fd, unix.TIOCSETD, nPPS); err != nil {
		t.Skipf("this kernel will not attach N_PPS to %s: %v (needs the pps_ldisc module)", slavePath, err)
	}
	// TIOCSETD can report success without changing anything if the kernel
	// treats the request as a no-op; only a confirmed N_PPS proves the point.
	disc, err := unix.IoctlGetInt(port.fd, unix.TIOCGETD)
	if err != nil {
		t.Skipf("cannot read back the line discipline of %s: %v", slavePath, err)
	}
	if disc != nPPS {
		t.Skipf("line discipline of %s is %d, not N_PPS", slavePath, disc)
	}

	// The claim under test. Write only after the attach: installing a
	// discipline frees the previous one's read buffer, so anything already
	// buffered is legitimately lost.
	const after = "$GPRMC,after,A*00\r\n"
	writeAll(t, master, after)
	got := readChunk(t, port)
	if got != after {
		t.Fatalf("N_PPS is attached and the pty delivered %q, want %q; "+
			"if this is real, the discipline no longer inherits the N_TTY "+
			"operations and a GPS cannot share its tty with the pulse", got, after)
	}
}

// openPTY returns the master descriptor of a fresh pseudo-terminal pair and
// the path of its slave. Both are closed when the test ends.
func openPTY(t *testing.T) (int, string) {
	t.Helper()
	master, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Skipf("cannot open /dev/ptmx: %v", err)
	}
	t.Cleanup(func() { _ = unix.Close(master) })
	if err := unix.IoctlSetPointerInt(master, unix.TIOCSPTLCK, 0); err != nil {
		t.Skipf("cannot unlock the pty slave: %v", err)
	}
	n, err := unix.IoctlGetInt(master, unix.TIOCGPTN)
	if err != nil {
		t.Skipf("cannot read the pty number: %v", err)
	}
	return master, fmt.Sprintf("/dev/pts/%d", n)
}

func writeAll(t *testing.T, fd int, s string) {
	t.Helper()
	for buf := []byte(s); len(buf) > 0; {
		n, err := unix.Write(fd, buf)
		if err != nil {
			if errors.Is(err, unix.EINTR) || errors.Is(err, unix.EAGAIN) {
				continue
			}
			t.Fatalf("writing to the pty master: %v", err)
		}
		buf = buf[n:]
	}
}

// readChunk collects what the port delivers within a second. A pty may split
// a write across reads, so it keeps reading until the line is terminated.
func readChunk(t *testing.T, p *Port) string {
	t.Helper()
	var got bytes.Buffer
	buf := make([]byte, 256)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		n, err := p.ReadTimeout(buf, 100*time.Millisecond)
		if errors.Is(err, ErrTimeout) {
			continue
		}
		if err != nil {
			t.Fatalf("reading %s: %v", p.path, err)
		}
		got.Write(buf[:n])
		if bytes.ContainsRune(buf[:n], '\n') {
			break
		}
	}
	return got.String()
}
