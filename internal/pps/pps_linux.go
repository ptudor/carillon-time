//go:build linux

package pps

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unsafe"

	"carillon/internal/serial"
	"golang.org/x/sys/unix"
)

const (
	ppsAPIVersion    = 1
	ppsCaptureAssert = 0x01
	ppsCaptureClear  = 0x02
	ppsTSFmtTSpec    = 0x1000
	ppsTimeInvalid   = 1
)

type ppsKTime struct {
	Sec   int64
	Nsec  int32
	Flags uint32
}

type ppsKInfo struct {
	AssertSequence uint32
	ClearSequence  uint32
	AssertTime     ppsKTime
	ClearTime      ppsKTime
	CurrentMode    int32
}

type ppsFData struct {
	Info    ppsKInfo
	Timeout ppsKTime
}

type ppsKParams struct {
	APIVersion   int32
	Mode         int32
	AssertOffset ppsKTime
	ClearOffset  ppsKTime
}

type device struct {
	fd   int
	edge Edge
	tty  *serial.Port
}

// Open opens a /dev/ppsN source, or attaches N_PPS when path names a tty.
func Open(path string, edge Edge) (Reader, error) {
	if edge != Assert && edge != Clear {
		return nil, fmt.Errorf("pps: invalid edge %d", edge)
	}
	ppsPath := path
	var tty *serial.Port
	var err error
	if !linuxPPSPath(path) {
		tty, ppsPath, err = serial.AttachPPS(path)
		if err != nil {
			return nil, err
		}
	}
	fd, err := unix.Open(ppsPath, unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		if tty != nil {
			_ = tty.Close()
		}
		return nil, fmt.Errorf("pps: open %s: %w", ppsPath, err)
	}
	d := &device{fd: fd, edge: edge, tty: tty}
	if err := d.configure(); err != nil {
		_ = d.Close()
		return nil, fmt.Errorf("pps: configure %s: %w", ppsPath, err)
	}
	return d, nil
}

// linuxPPSPath reports whether path names a kernel PPS device rather than a
// tty that needs the N_PPS line discipline attached to it.
//
// The name is only a hint. An operator's stable alias — a udev symlink such
// as /dev/pps-gps — was classified as a tty and the daemon then tried to
// attach N_PPS to a PPS device, which fails (RA6X-039). So the path is
// resolved first, and if the resolved name still does not look like a PPS
// device the kernel is asked directly: every registered PPS device publishes
// its device number under /sys/class/pps/ppsN/dev, and a character device
// whose number is listed there is a PPS device whatever it is called.
func linuxPPSPath(path string) bool {
	resolved := path
	if r, err := filepath.EvalSymlinks(path); err == nil {
		resolved = r
	}
	if ppsBasename(resolved) || ppsBasename(path) {
		return true
	}
	return registeredPPSDevice(resolved)
}

func ppsBasename(path string) bool {
	base := filepath.Base(path)
	if !strings.HasPrefix(base, "pps") {
		return false
	}
	_, err := strconv.ParseUint(strings.TrimPrefix(base, "pps"), 10, 32)
	return err == nil
}

// registeredPPSDevice compares the path's device number against the ones the
// kernel lists under /sys/class/pps. It is read-only and opens no device.
func registeredPPSDevice(path string) bool {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFCHR {
		return false
	}
	want := fmt.Sprintf("%d:%d", unix.Major(uint64(st.Rdev)), unix.Minor(uint64(st.Rdev)))
	entries, err := os.ReadDir("/sys/class/pps")
	if err != nil {
		return false
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join("/sys/class/pps", e.Name(), "dev"))
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(b)) == want {
			return true
		}
	}
	return false
}

func (d *device) configure() error {
	var caps int32
	if err := ioctl(d.fd, uintptr(unix.PPS_GETCAP), unsafe.Pointer(&caps)); err != nil {
		return fmt.Errorf("get capabilities: %w", err)
	}
	want := int32(ppsCaptureAssert)
	if d.edge == Clear {
		want = ppsCaptureClear
	}
	if caps&want == 0 {
		return fmt.Errorf("edge %s is not supported (capabilities %#x)", d.edge, caps)
	}
	params := ppsKParams{APIVersion: ppsAPIVersion, Mode: want | ppsTSFmtTSpec}
	if err := ioctl(d.fd, uintptr(unix.PPS_SETPARAMS), unsafe.Pointer(&params)); err != nil {
		return fmt.Errorf("set parameters: %w", err)
	}
	return nil
}

func (d *device) Fetch(timeout time.Duration) (Sample, error) {
	var data ppsFData
	if timeout < 0 {
		data.Timeout.Flags = ppsTimeInvalid
	} else {
		data.Timeout.Sec = int64(timeout / time.Second)
		data.Timeout.Nsec = int32(timeout % time.Second)
	}
	if err := ioctl(d.fd, uintptr(unix.PPS_FETCH), unsafe.Pointer(&data)); err != nil {
		if errors.Is(err, unix.ETIMEDOUT) || errors.Is(err, unix.EAGAIN) {
			return Sample{}, ErrTimeout
		}
		return Sample{}, fmt.Errorf("pps: fetch: %w", err)
	}
	seq, ts := data.Info.AssertSequence, data.Info.AssertTime
	if d.edge == Clear {
		seq, ts = data.Info.ClearSequence, data.Info.ClearTime
	}
	return Sample{Sequence: seq, Time: time.Unix(ts.Sec, int64(ts.Nsec))}, nil
}

func (d *device) Close() error {
	if d == nil {
		return nil
	}
	var errs []error
	if d.fd >= 0 {
		if err := unix.Close(d.fd); err != nil {
			errs = append(errs, err)
		}
		d.fd = -1
	}
	if d.tty != nil {
		if err := d.tty.Close(); err != nil {
			errs = append(errs, err)
		}
		d.tty = nil
	}
	return errors.Join(errs...)
}

func ioctl(fd int, req uintptr, arg unsafe.Pointer) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), req, uintptr(arg))
	runtime.KeepAlive(arg)
	if errno != 0 {
		return errno
	}
	return nil
}
