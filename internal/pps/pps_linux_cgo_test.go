//go:build linux && cgo && hwtest

package pps

/*
#include <linux/pps.h>
*/
import "C"

import (
	"testing"
	"unsafe"
)

func TestLinuxLayouts(t *testing.T) {
	var gt ppsKTime
	var ct C.struct_pps_ktime
	checkSize(t, "pps_ktime", unsafe.Sizeof(gt), unsafe.Sizeof(ct))
	checkOffset(t, "pps_ktime.sec", unsafe.Offsetof(gt.Sec), unsafe.Offsetof(ct.sec))
	checkOffset(t, "pps_ktime.nsec", unsafe.Offsetof(gt.Nsec), unsafe.Offsetof(ct.nsec))
	checkOffset(t, "pps_ktime.flags", unsafe.Offsetof(gt.Flags), unsafe.Offsetof(ct.flags))

	var gi ppsKInfo
	var ci C.struct_pps_kinfo
	checkSize(t, "pps_kinfo", unsafe.Sizeof(gi), unsafe.Sizeof(ci))
	checkOffset(t, "pps_kinfo.assert_sequence", unsafe.Offsetof(gi.AssertSequence), unsafe.Offsetof(ci.assert_sequence))
	checkOffset(t, "pps_kinfo.assert_tu", unsafe.Offsetof(gi.AssertTime), unsafe.Offsetof(ci.assert_tu))
	checkOffset(t, "pps_kinfo.current_mode", unsafe.Offsetof(gi.CurrentMode), unsafe.Offsetof(ci.current_mode))

	var gf ppsFData
	var cf C.struct_pps_fdata
	checkSize(t, "pps_fdata", unsafe.Sizeof(gf), unsafe.Sizeof(cf))
	checkOffset(t, "pps_fdata.timeout", unsafe.Offsetof(gf.Timeout), unsafe.Offsetof(cf.timeout))

	var gp ppsKParams
	var cp C.struct_pps_kparams
	checkSize(t, "pps_kparams", unsafe.Sizeof(gp), unsafe.Sizeof(cp))
	checkOffset(t, "pps_kparams.assert_off_tu", unsafe.Offsetof(gp.AssertOffset), unsafe.Offsetof(cp.assert_off_tu))
}

func checkSize(t *testing.T, name string, got, want uintptr) {
	t.Helper()
	if got != want {
		t.Errorf("sizeof %s: Go %d, C %d", name, got, want)
	}
}

func checkOffset(t *testing.T, name string, got, want uintptr) {
	t.Helper()
	if got != want {
		t.Errorf("offset of %s: Go %d, C %d", name, got, want)
	}
}
