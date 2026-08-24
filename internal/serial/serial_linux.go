//go:build linux

package serial

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const nPPS = 18

// OpenCLOCAL opens a tty and sets CLOCAL before doing anything else that
// could observe a DCD drop. Linux can report EIO if carrier changed during
// the first open, so that narrow race is retried once.
func OpenCLOCAL(path string) (*Port, error) {
	p, err := openCLOCAL(path, false)
	if errors.Is(err, unix.EIO) {
		p, err = openCLOCAL(path, false)
	}
	return p, err
}

func openCLOCAL(path string, keepNonblock bool) (*Port, error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("serial: open %s: %w", path, err)
	}
	term, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err == nil {
		term.Cflag |= unix.CLOCAL
		err = unix.IoctlSetTermios(fd, unix.TCSETS, term)
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

// OpenNMEA opens a tty in raw 8N1 mode at baud. It remains non-blocking and
// is consumed with Port.ReadTimeout.
func OpenNMEA(path string, baud int) (*Port, error) {
	p, err := openCLOCAL(path, true)
	if errors.Is(err, unix.EIO) {
		p, err = openCLOCAL(path, true)
	}
	if err != nil {
		return nil, err
	}
	term, err := unix.IoctlGetTermios(p.fd, unix.TCGETS)
	if err == nil {
		speed, ok := linuxBaud(baud)
		if !ok {
			err = fmt.Errorf("unsupported baud %d", baud)
		} else {
			term.Iflag = 0
			term.Oflag = 0
			term.Lflag = 0
			term.Cflag &^= unix.CSIZE | unix.PARENB | unix.CSTOPB | unix.CRTSCTS | unix.CBAUD
			term.Cflag |= unix.CS8 | unix.CLOCAL | unix.CREAD | speed
			term.Ispeed, term.Ospeed = speed, speed
			term.Cc[unix.VMIN], term.Cc[unix.VTIME] = 1, 0
			err = unix.IoctlSetTermios(p.fd, unix.TCSETS, term)
		}
	}
	if err != nil {
		_ = p.Close()
		return nil, fmt.Errorf("serial: configure NMEA on %s: %w", path, err)
	}
	return p, nil
}

func linuxBaud(baud int) (uint32, bool) {
	switch baud {
	case 4800:
		return unix.B4800, true
	case 9600:
		return unix.B9600, true
	case 19200:
		return unix.B19200, true
	case 38400:
		return unix.B38400, true
	case 57600:
		return unix.B57600, true
	case 115200:
		return unix.B115200, true
	default:
		return 0, false
	}
}

// AttachPPS installs the N_PPS line discipline, waits for its /dev/ppsN
// device to appear, and returns both that device and the tty which must stay
// open to keep the discipline attached.
func AttachPPS(path string) (*Port, string, error) {
	p, err := OpenCLOCAL(path)
	if err != nil {
		return nil, "", err
	}
	if err := unix.IoctlSetPointerInt(p.fd, unix.TIOCSETD, nPPS); err != nil {
		_ = p.Close()
		return nil, "", fmt.Errorf("serial: attach N_PPS to %s: %w (load the pps_ldisc module)", path, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if ppsPath := findPPSDevice(path); ppsPath != "" {
			return p, ppsPath, nil
		}
		if time.Now().After(deadline) {
			_ = p.Close()
			return nil, "", fmt.Errorf("serial: N_PPS attached to %s but no matching /dev/ppsN appeared", path)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func findPPSDevice(tty string) string {
	want, err := filepath.EvalSymlinks(tty)
	if err != nil {
		want = tty
	}
	paths, _ := filepath.Glob("/sys/class/pps/pps*/path")
	for _, pathFile := range paths {
		b, err := os.ReadFile(pathFile)
		if err != nil {
			continue
		}
		got := strings.TrimSpace(string(b))
		if resolved, err := filepath.EvalSymlinks(got); err == nil {
			got = resolved
		}
		if filepath.Clean(got) == filepath.Clean(want) {
			return filepath.Join("/dev", filepath.Base(filepath.Dir(pathFile)))
		}
	}
	return ""
}

// Close releases the tty. If N_PPS was attached, this also detaches it.
func (p *Port) Close() error {
	if p == nil || p.fd < 0 {
		return nil
	}
	err := unix.Close(p.fd)
	p.fd = -1
	return err
}
