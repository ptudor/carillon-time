//go:build linux || freebsd

package serial

import (
	"errors"
	"fmt"
	"io"
	"time"

	"golang.org/x/sys/unix"
)

// ReadTimeout waits up to timeout and reads one available chunk. The port is
// kept non-blocking so shutdown never depends on another serial byte arriving.
func (p *Port) ReadTimeout(buf []byte, timeout time.Duration) (int, error) {
	ms := int(timeout / time.Millisecond)
	if timeout > 0 && ms == 0 {
		ms = 1
	}
	fds := []unix.PollFd{{Fd: int32(p.fd), Events: unix.POLLIN}}
	n, err := p.pollFn()(fds, ms)
	if err != nil {
		if errors.Is(err, unix.EINTR) {
			return 0, ErrTimeout
		}
		return 0, fmt.Errorf("serial: poll %s: %w", p.path, err)
	}
	if n == 0 {
		return 0, ErrTimeout
	}
	if fds[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
		return 0, fmt.Errorf("serial: poll %s: device unavailable (events %#x)", p.path, fds[0].Revents)
	}
	n, err = p.readFn()(p.fd, buf)
	if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) {
		return 0, ErrTimeout
	}
	if err != nil {
		return 0, fmt.Errorf("serial: read %s: %w", p.path, err)
	}
	if n == 0 {
		// poll(2) said readable and read(2) returned nothing: end of file.
		// A CLOCAL tty should never do this and a detached device should
		// surface as POLLHUP or ENXIO above, but some USB-serial drivers
		// return a single zero-length read after a hangup before EIO.
		// Reporting it as success makes the caller poll again, find the
		// descriptor readable-at-EOF at once, and spin at 100 % CPU with
		// nothing in the log — on the GPS host, which is also the PPS host.
		return 0, fmt.Errorf("serial: read %s: %w", p.path, io.EOF)
	}
	return n, nil
}

// pollFn and readFn return the syscall seam, defaulting to the real calls.
func (p *Port) pollFn() func([]unix.PollFd, int) (int, error) {
	if p.poll != nil {
		return p.poll
	}
	return unix.Poll
}

func (p *Port) readFn() func(int, []byte) (int, error) {
	if p.read != nil {
		return p.read
	}
	return unix.Read
}
