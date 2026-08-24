//go:build freebsd

package serial

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// OpenCLOCAL opens a FreeBSD callout tty and immediately makes carrier local
// so that a DCD PPS edge cannot hang it up.
func OpenCLOCAL(path string) (*Port, error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("serial: open %s: %w", path, err)
	}
	term, err := unix.IoctlGetTermios(fd, unix.TIOCGETA)
	if err == nil {
		term.Cflag |= unix.CLOCAL
		err = unix.IoctlSetTermios(fd, unix.TIOCSETA, term)
	}
	if err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("serial: set CLOCAL on %s: %w", path, err)
	}
	return &Port{fd: fd, path: path}, nil
}

// Close releases the callout tty.
func (p *Port) Close() error {
	if p == nil || p.fd < 0 {
		return nil
	}
	err := unix.Close(p.fd)
	p.fd = -1
	return err
}
