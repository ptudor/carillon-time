//go:build freebsd

package serial

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// OpenCLOCAL opens a FreeBSD callout tty and immediately makes carrier local
// so that a DCD PPS edge cannot hang it up.
func OpenCLOCAL(path string) (*Port, error) {
	return openCLOCAL(path, false)
}

func openCLOCAL(path string, keepNonblock bool) (*Port, error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
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
	if !keepNonblock {
		flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
		if err == nil {
			_, err = unix.FcntlInt(uintptr(fd), unix.F_SETFL, flags&^unix.O_NONBLOCK)
		}
		if err != nil {
			_ = unix.Close(fd)
			return nil, fmt.Errorf("serial: clear O_NONBLOCK on %s: %w", path, err)
		}
	}
	return &Port{fd: fd, path: path}, nil
}

// OpenNMEA opens a FreeBSD callout tty in raw 8N1 mode at baud.
func OpenNMEA(path string, baud int) (*Port, error) {
	p, err := openCLOCAL(path, true)
	if err != nil {
		return nil, err
	}
	if !validBaud(baud) {
		_ = p.Close()
		return nil, fmt.Errorf("serial: configure NMEA on %s: unsupported baud %d", path, baud)
	}
	term, err := unix.IoctlGetTermios(p.fd, unix.TIOCGETA)
	if err == nil {
		term.Iflag = 0
		term.Oflag = 0
		term.Lflag = 0
		term.Cflag &^= unix.CSIZE | unix.PARENB | unix.CSTOPB | unix.CRTSCTS
		term.Cflag |= unix.CS8 | unix.CLOCAL | unix.CREAD
		term.Ispeed, term.Ospeed = uint32(baud), uint32(baud)
		term.Cc[unix.VMIN], term.Cc[unix.VTIME] = 1, 0
		err = unix.IoctlSetTermios(p.fd, unix.TIOCSETA, term)
	}
	if err != nil {
		_ = p.Close()
		return nil, fmt.Errorf("serial: configure NMEA on %s: %w", path, err)
	}
	return p, nil
}

func validBaud(baud int) bool {
	switch baud {
	case 4800, 9600, 19200, 38400, 57600, 115200:
		return true
	default:
		return false
	}
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
