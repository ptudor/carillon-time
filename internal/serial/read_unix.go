//go:build linux || freebsd

package serial

import (
	"errors"
	"fmt"
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
	n, err := unix.Poll(fds, ms)
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
	n, err = unix.Read(p.fd, buf)
	if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) {
		return 0, ErrTimeout
	}
	if err != nil {
		return 0, fmt.Errorf("serial: read %s: %w", p.path, err)
	}
	return n, nil
}
