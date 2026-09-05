//go:build linux && cgo && abicheck

// Package-level C layout probe for the hand-declared Linux PPS structs.
//
// The kernel ABI is declared by hand in pps_linux.go because the daemon is
// built with CGO_ENABLED=0 (see CLAUDE.md, "No cgo"). This file is the only
// place a C header is consulted, and it exists solely so the layout can be
// compared against <linux/pps.h> on a native host. Go refuses `import "C"`
// inside a _test.go file, so the C side lives here and the comparison lives
// in abi_linux_test.go, which is ordinary Go.
//
// Build tag `abicheck` is deliberately distinct from `hwtest`: reading header
// offsets touches no device and no clock, so this check must not require
// enabling the tests that do. Run it natively with:
//
//	CGO_ENABLED=1 go test -tags abicheck ./internal/pps/ -run TestLinuxLayouts

package pps

/*
#include <linux/pps.h>
*/
import "C"

import (
	"unsafe"

	"golang.org/x/sys/unix"
)

// cLayout is one C-header fact: a size, an offset, or a constant.
type cLayout struct {
	name string
	got  uintptr // the hand-declared Go value
	want uintptr // the value the C header says
}

// cLinuxLayouts returns every checked fact about <linux/pps.h>.
func cLinuxLayouts() []cLayout {
	var gt ppsKTime
	var ct C.struct_pps_ktime
	var gi ppsKInfo
	var ci C.struct_pps_kinfo
	var gf ppsFData
	var cf C.struct_pps_fdata
	var gp ppsKParams
	var cp C.struct_pps_kparams

	return []cLayout{
		{"sizeof pps_ktime", unsafe.Sizeof(gt), unsafe.Sizeof(ct)},
		{"pps_ktime.sec", unsafe.Offsetof(gt.Sec), unsafe.Offsetof(ct.sec)},
		{"pps_ktime.nsec", unsafe.Offsetof(gt.Nsec), unsafe.Offsetof(ct.nsec)},
		{"pps_ktime.flags", unsafe.Offsetof(gt.Flags), unsafe.Offsetof(ct.flags)},

		{"sizeof pps_kinfo", unsafe.Sizeof(gi), unsafe.Sizeof(ci)},
		{"pps_kinfo.assert_sequence", unsafe.Offsetof(gi.AssertSequence), unsafe.Offsetof(ci.assert_sequence)},
		{"pps_kinfo.clear_sequence", unsafe.Offsetof(gi.ClearSequence), unsafe.Offsetof(ci.clear_sequence)},
		{"pps_kinfo.assert_tu", unsafe.Offsetof(gi.AssertTime), unsafe.Offsetof(ci.assert_tu)},
		{"pps_kinfo.clear_tu", unsafe.Offsetof(gi.ClearTime), unsafe.Offsetof(ci.clear_tu)},
		{"pps_kinfo.current_mode", unsafe.Offsetof(gi.CurrentMode), unsafe.Offsetof(ci.current_mode)},

		{"sizeof pps_fdata", unsafe.Sizeof(gf), unsafe.Sizeof(cf)},
		{"pps_fdata.info", unsafe.Offsetof(gf.Info), unsafe.Offsetof(cf.info)},
		{"pps_fdata.timeout", unsafe.Offsetof(gf.Timeout), unsafe.Offsetof(cf.timeout)},

		{"sizeof pps_kparams", unsafe.Sizeof(gp), unsafe.Sizeof(cp)},
		{"pps_kparams.api_version", unsafe.Offsetof(gp.APIVersion), unsafe.Offsetof(cp.api_version)},
		{"pps_kparams.mode", unsafe.Offsetof(gp.Mode), unsafe.Offsetof(cp.mode)},
		{"pps_kparams.assert_off_tu", unsafe.Offsetof(gp.AssertOffset), unsafe.Offsetof(cp.assert_off_tu)},
		{"pps_kparams.clear_off_tu", unsafe.Offsetof(gp.ClearOffset), unsafe.Offsetof(cp.clear_off_tu)},

		{"PPS_API_VERS_1", ppsAPIVersion, uintptr(C.PPS_API_VERS_1)},
		{"PPS_CAPTUREASSERT", ppsCaptureAssert, uintptr(C.PPS_CAPTUREASSERT)},
		{"PPS_CAPTURECLEAR", ppsCaptureClear, uintptr(C.PPS_CAPTURECLEAR)},
		{"PPS_TSFMT_TSPEC", ppsTSFmtTSpec, uintptr(C.PPS_TSFMT_TSPEC)},
		{"PPS_TIME_INVALID", ppsTimeInvalid, uintptr(C.PPS_TIME_INVALID)},
		// The ioctl numbers come from x/sys rather than being hand-declared;
		// compare them anyway, since a mismatch is just as fatal.
		{"PPS_GETPARAMS", uintptr(unix.PPS_GETPARAMS), uintptr(C.PPS_GETPARAMS)},
		{"PPS_SETPARAMS", uintptr(unix.PPS_SETPARAMS), uintptr(C.PPS_SETPARAMS)},
		{"PPS_GETCAP", uintptr(unix.PPS_GETCAP), uintptr(C.PPS_GETCAP)},
		{"PPS_FETCH", uintptr(unix.PPS_FETCH), uintptr(C.PPS_FETCH)},
	}
}
