//go:build freebsd && (amd64 || arm64)

package clock

import "unsafe"

// timex mirrors struct timex from FreeBSD's <sys/timex.h> on LP64
// architectures, which are the only FreeBSD targets carillon supports. The
// build constraint makes a 32-bit FreeBSD build fail to compile rather than
// pass a mis-sized struct to the kernel. The hwtest-tagged cgo test compares
// this layout against the C header on a real FreeBSD host.
type timex struct {
	Modes     uint32 // clock mode bits (wo)
	_         [4]byte
	Offset    int64 // time offset (us) (rw)
	Freq      int64 // frequency offset (scaled ppm) (rw)
	Maxerror  int64 // maximum error (us) (rw)
	Esterror  int64 // estimated error (us) (rw)
	Status    int32 // clock status bits (rw)
	_         [4]byte
	Constant  int64 // pll time constant (rw)
	Precision int64 // clock precision (us) (ro)
	Tolerance int64 // clock frequency tolerance (scaled ppm) (ro)
	Ppsfreq   int64 // pps frequency (scaled ppm) (ro)
	Jitter    int64 // pps jitter (us) (ro)
	Shift     int32 // interval duration (s) (shift) (ro)
	_         [4]byte
	Stabil    int64 // pps stability (scaled ppm) (ro)
	Jitcnt    int64 // jitter limit exceeded (ro)
	Calcnt    int64 // calibration intervals (ro)
	Errcnt    int64 // calibration errors (ro)
	Stbcnt    int64 // stability limit exceeded (ro)
}

// sizeofTimex is sizeof(struct timex) on FreeBSD LP64.
const sizeofTimex = 136

// A mismatch between the declared layout and the expected size fails to
// compile.
var _ [sizeofTimex]byte = [unsafe.Sizeof(timex{})]byte{}

// ntp_adjtime(2) mode bits (<sys/timex.h>).
const (
	modOffset    = 0x0001 // set time offset
	modFrequency = 0x0002 // set frequency offset
	modMaxerror  = 0x0004 // set maximum time error
	modEsterror  = 0x0008 // set estimated time error
	modStatus    = 0x0010 // set clock status bits
	modTimeconst = 0x0020 // set pll time constant
	modNano      = 0x2000 // select nanosecond resolution
)

// staNano is the read-only status bit reporting nanosecond resolution; it
// is listed so the layout test can name every bit the daemon touches.
const staNano = 0x2000
