//go:build linux || freebsd

package serial

import (
	"errors"
	"io"
	"os"
	"testing"
	"time"
)

// TestReadTimeoutReportsEOF covers RF5X-013. A zero-byte read with no error
// used to be reported as a successful read of nothing, which made NMEA.Run
// loop straight back into poll(2), find the descriptor readable-at-EOF, and
// spin at 100 % CPU with no log line. It must surface as an error so the
// caller takes its reopen path.
func TestReadTimeoutReportsEOF(t *testing.T) {
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
	n, err := p.ReadTimeout(buf, 100*time.Millisecond)
	if n != 0 {
		t.Fatalf("read %d bytes from a closed pipe", n)
	}
	if !errors.Is(err, io.EOF) {
		t.Fatalf("error %v, want an io.EOF", err)
	}
	if errors.Is(err, ErrTimeout) {
		t.Fatal("EOF must not be reported as a timeout: the caller would keep polling")
	}
}
