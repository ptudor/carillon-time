//go:build linux || freebsd

package serial

import (
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestReadTimeoutClosedPipeIsNotATimeout covers RA6X-056. Closing the write
// end of a pipe makes poll(2) report POLLHUP, which ReadTimeout classifies as
// a device-unavailable error before it ever calls read(2). The point of the
// test is that a disconnect is reported promptly as a non-timeout error, so
// NMEA.Run takes its reopen path; asserting io.EOF here asserted the wrong
// kernel event and failed on the supported hosts.
func TestReadTimeoutClosedPipeIsNotATimeout(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := w.Close(); err != nil { // readable, and every read is EOF
		t.Fatal(err)
	}
	p := &Port{fd: int(r.Fd()), path: "pipe"}
	buf := make([]byte, 64)
	start := time.Now()
	n, err := p.ReadTimeout(buf, 2*time.Second)
	if n != 0 {
		t.Fatalf("read %d bytes from a closed pipe", n)
	}
	if err == nil {
		t.Fatal("a disconnected descriptor must not read successfully")
	}
	if errors.Is(err, ErrTimeout) {
		t.Fatalf("disconnect reported as a timeout (%v): the caller would keep polling", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("disconnect took %v to report; it must not wait out the timeout", elapsed)
	}
}

// TestReadTimeoutReportsEOF covers RF5X-013 and RA6X-056. A zero-byte read
// with no error after a readable poll used to be reported as a successful
// read of nothing, which made NMEA.Run loop straight back into poll(2), find
// the descriptor readable-at-EOF, and spin at 100 % CPU with no log line. No
// pipe produces that pair, so the syscall seam produces it directly.
func TestReadTimeoutReportsEOF(t *testing.T) {
	p := &Port{
		fd:   -1,
		path: "seam",
		poll: func(fds []unix.PollFd, _ int) (int, error) {
			fds[0].Revents = unix.POLLIN
			return 1, nil
		},
		read: func(int, []byte) (int, error) { return 0, nil },
	}
	buf := make([]byte, 64)
	n, err := p.ReadTimeout(buf, 100*time.Millisecond)
	if n != 0 {
		t.Fatalf("read %d bytes, want 0", n)
	}
	if !errors.Is(err, io.EOF) {
		t.Fatalf("error %v, want an io.EOF", err)
	}
	if errors.Is(err, ErrTimeout) {
		t.Fatal("EOF must not be reported as a timeout: the caller would keep polling")
	}
}

// TestReadTimeoutHangupIsNotEOF pins the production classification the
// closed-pipe case actually exercises: POLLHUP is a device error raised
// before read(2), not an end of file.
func TestReadTimeoutHangupIsNotEOF(t *testing.T) {
	called := false
	p := &Port{
		fd:   -1,
		path: "seam",
		poll: func(fds []unix.PollFd, _ int) (int, error) {
			fds[0].Revents = unix.POLLIN | unix.POLLHUP
			return 1, nil
		},
		read: func(int, []byte) (int, error) { called = true; return 0, nil },
	}
	_, err := p.ReadTimeout(make([]byte, 64), 100*time.Millisecond)
	if called {
		t.Fatal("read(2) called after poll reported HUP")
	}
	if err == nil || errors.Is(err, ErrTimeout) || errors.Is(err, io.EOF) {
		t.Fatalf("HUP error %v, want a non-timeout device error that is not io.EOF", err)
	}
}

// TestReadTimeoutStillTimesOut checks the seam did not disturb the ordinary
// no-data path, which must stay a bounded ErrTimeout.
func TestReadTimeoutStillTimesOut(t *testing.T) {
	p := &Port{
		fd:   -1,
		path: "seam",
		poll: func([]unix.PollFd, int) (int, error) { return 0, nil },
		read: func(int, []byte) (int, error) { t.Error("read(2) called after poll timed out"); return 0, nil },
	}
	if _, err := p.ReadTimeout(make([]byte, 64), 100*time.Millisecond); !errors.Is(err, ErrTimeout) {
		t.Fatalf("error %v, want ErrTimeout", err)
	}
}
