//go:build freebsd && (amd64 || arm64)

package pps

import (
	"errors"
	"fmt"
	"runtime"
	"time"
	"unsafe"

	"github.com/ptudor/carillon-time/internal/serial"
	"golang.org/x/sys/unix"
)

const (
	ppsAPIVersion    = 1
	ppsCaptureAssert = 0x01
	ppsCaptureClear  = 0x02
	ppsTSFmtTSpec    = 0x1000

	ppsIOCCreate    = 0x20003101
	ppsIOCDestroy   = 0x20003102
	ppsIOCSetParams = 0x80383103
	ppsIOCGetCap    = 0x40043105
	ppsIOCFetch     = 0xc0583106
)

// ppsTimeU is the timespec member of FreeBSD's 24-byte pps_timeu_t union.
type ppsTimeU struct {
	Sec  int64
	Nsec int64
	_    int64
}

type ppsInfo struct {
	AssertSequence uint32
	ClearSequence  uint32
	AssertTime     ppsTimeU
	ClearTime      ppsTimeU
	CurrentMode    int32
	_              [4]byte
}

type ppsParams struct {
	APIVersion   int32
	Mode         int32
	AssertOffset ppsTimeU
	ClearOffset  ppsTimeU
}

type ppsFetchArgs struct {
	TSFormat int32
	_        [4]byte
	Info     ppsInfo
	Timeout  unix.Timespec
}

const (
	sizeofPPSInfo  = 64
	sizeofPPSFetch = 88
)

var (
	_ [sizeofPPSInfo]byte  = [unsafe.Sizeof(ppsInfo{})]byte{}
	_ [sizeofPPSFetch]byte = [unsafe.Sizeof(ppsFetchArgs{})]byte{}
)

type device struct {
	port *serial.Port
	edge Edge
}

// Open opens a FreeBSD callout tty and creates its RFC 2783 PPS handle.
func Open(path string, edge Edge) (Reader, error) {
	if edge != Assert && edge != Clear {
		return nil, fmt.Errorf("pps: invalid edge %d", edge)
	}
	port, err := serial.OpenCLOCAL(path)
	if err != nil {
		return nil, err
	}
	d := &device{port: port, edge: edge}
	if err := ioctl(port.FD(), ppsIOCCreate, nil); err != nil {
		_ = port.Close()
		return nil, fmt.Errorf("pps: create handle on %s: %w", path, err)
	}
	if err := d.configure(); err != nil {
		_ = d.Close()
		return nil, fmt.Errorf("pps: configure %s: %w", path, err)
	}
	return d, nil
}

func (d *device) configure() error {
	var caps int32
	if err := ioctl(d.port.FD(), ppsIOCGetCap, unsafe.Pointer(&caps)); err != nil {
		return fmt.Errorf("get capabilities: %w", err)
	}
	want := int32(ppsCaptureAssert)
	if d.edge == Clear {
		want = ppsCaptureClear
	}
	if caps&want == 0 {
		return fmt.Errorf("edge %s is not supported (capabilities %#x)", d.edge, caps)
	}
	params := ppsParams{APIVersion: ppsAPIVersion, Mode: want | ppsTSFmtTSpec}
	if err := ioctl(d.port.FD(), ppsIOCSetParams, unsafe.Pointer(&params)); err != nil {
		return fmt.Errorf("set parameters: %w", err)
	}
	return nil
}

func (d *device) Fetch(timeout time.Duration) (Sample, error) {
	args := ppsFetchArgs{TSFormat: ppsTSFmtTSpec}
	if timeout < 0 {
		args.Timeout.Sec = -1
		args.Timeout.Nsec = -1
	} else {
		args.Timeout = unix.NsecToTimespec(timeout.Nanoseconds())
	}
	if err := ioctl(d.port.FD(), ppsIOCFetch, unsafe.Pointer(&args)); err != nil {
		if errors.Is(err, unix.ETIMEDOUT) || errors.Is(err, unix.EAGAIN) {
			return Sample{}, ErrTimeout
		}
		return Sample{}, fmt.Errorf("pps: fetch: %w", err)
	}
	seq, ts := args.Info.AssertSequence, args.Info.AssertTime
	if d.edge == Clear {
		seq, ts = args.Info.ClearSequence, args.Info.ClearTime
	}
	return Sample{Sequence: seq, Time: time.Unix(ts.Sec, ts.Nsec)}, nil
}

func (d *device) Close() error {
	if d == nil || d.port == nil {
		return nil
	}
	var errs []error
	if err := ioctl(d.port.FD(), ppsIOCDestroy, nil); err != nil {
		errs = append(errs, err)
	}
	if err := d.port.Close(); err != nil {
		errs = append(errs, err)
	}
	d.port = nil
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
