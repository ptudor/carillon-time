// Package serial opens serial ports without letting a modem-control PPS
// signal hang up the descriptor. Platform files add the Linux PPS line
// discipline where needed.
package serial

import (
	"errors"

	"golang.org/x/sys/unix"
)

// ErrTimeout means no serial data was readable before the requested timeout.
var ErrTimeout = errors.New("serial read timed out")

// Port is an open serial port whose descriptor is held for the lifetime of
// a reference clock.
type Port struct {
	fd   int
	path string

	// poll and read are the syscall seam ReadTimeout uses. They are nil in
	// production, where the real unix.Poll and unix.Read are called; only
	// tests set them, to drive kernel outcomes a pipe cannot produce (a
	// readable descriptor whose read(2) returns n=0 with no error). A Port
	// is owned by one reference-clock goroutine, so these need no locking.
	poll func(fds []unix.PollFd, timeoutMillis int) (int, error)
	read func(fd int, buf []byte) (int, error)
}

// FD returns the kernel descriptor.
func (p *Port) FD() int { return p.fd }

// Path returns the configured device path.
func (p *Port) Path() string { return p.path }
