//go:build freebsd && cgo && hwtest && (amd64 || arm64)

package pps

/*
#include <stdint.h>
#include <sys/timepps.h>
*/
import "C"

import (
	"testing"
	"unsafe"
)

func TestFreeBSDLayouts(t *testing.T) {
	var gi ppsInfo
	var ci C.pps_info_t
	checkSize(t, "pps_info", unsafe.Sizeof(gi), unsafe.Sizeof(ci))
	checkOffset(t, "pps_info.assert_tu", unsafe.Offsetof(gi.AssertTime), unsafe.Offsetof(ci.assert_tu))
	checkOffset(t, "pps_info.clear_tu", unsafe.Offsetof(gi.ClearTime), unsafe.Offsetof(ci.clear_tu))
	checkOffset(t, "pps_info.current_mode", unsafe.Offsetof(gi.CurrentMode), unsafe.Offsetof(ci.current_mode))

	var gp ppsParams
	var cp C.pps_params_t
	checkSize(t, "pps_params", unsafe.Sizeof(gp), unsafe.Sizeof(cp))
	checkOffset(t, "pps_params.assert_off_tu", unsafe.Offsetof(gp.AssertOffset), unsafe.Offsetof(cp.assert_off_tu))

	var gf ppsFetchArgs
	var cf C.struct_pps_fetch_args
	checkSize(t, "pps_fetch_args", unsafe.Sizeof(gf), unsafe.Sizeof(cf))
	checkOffset(t, "pps_fetch_args.pps_info_buf", unsafe.Offsetof(gf.Info), unsafe.Offsetof(cf.pps_info_buf))
	checkOffset(t, "pps_fetch_args.timeout", unsafe.Offsetof(gf.Timeout), unsafe.Offsetof(cf.timeout))

	constants := []struct {
		name      string
		got, want uintptr
	}{
		{"PPS_IOC_CREATE", ppsIOCCreate, uintptr(C.PPS_IOC_CREATE)},
		{"PPS_IOC_DESTROY", ppsIOCDestroy, uintptr(C.PPS_IOC_DESTROY)},
		{"PPS_IOC_SETPARAMS", ppsIOCSetParams, uintptr(C.PPS_IOC_SETPARAMS)},
		{"PPS_IOC_GETCAP", ppsIOCGetCap, uintptr(C.PPS_IOC_GETCAP)},
		{"PPS_IOC_FETCH", ppsIOCFetch, uintptr(C.PPS_IOC_FETCH)},
	}
	for _, c := range constants {
		if c.got != c.want {
			t.Errorf("%s: Go %#x, C %#x", c.name, c.got, c.want)
		}
	}
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
