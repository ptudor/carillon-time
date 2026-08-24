//go:build linux

package pps

import (
	"errors"
	"fmt"
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

func linuxPPSPath(path string) bool {
	base := filepath.Base(path)
	if !strings.HasPrefix(base, "pps") {
		return false
	}
	_, err := strconv.ParseUint(strings.TrimPrefix(base, "pps"), 10, 32)
	return err == nil
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
