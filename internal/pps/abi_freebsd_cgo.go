//go:build freebsd && cgo && abicheck && (amd64 || arm64)

// Package-level C layout probe for the hand-declared FreeBSD PPS structs.
// See abi_linux_cgo.go for why the C side cannot live in a _test.go file and
// why the tag is abicheck rather than hwtest. Run natively with:
//
//	CGO_ENABLED=1 go test -tags abicheck ./internal/pps/ -run TestFreeBSDLayouts

package pps

/*
#include <stdint.h>
#include <sys/timepps.h>
*/
import "C"

import "unsafe"

// cLayout is one C-header fact: a size, an offset, or a constant.
type cLayout struct {
	name string
	got  uintptr // the hand-declared Go value
	want uintptr // the value the C header says
}

// cFreeBSDLayouts returns every checked fact about <sys/timepps.h>.
func cFreeBSDLayouts() []cLayout {
	var gi ppsInfo
	var ci C.pps_info_t
	var gp ppsParams
	var cp C.pps_params_t
	var gf ppsFetchArgs
	var cf C.struct_pps_fetch_args

	return []cLayout{
		{"sizeof pps_info", unsafe.Sizeof(gi), unsafe.Sizeof(ci)},
		{"pps_info.assert_sequence", unsafe.Offsetof(gi.AssertSequence), unsafe.Offsetof(ci.assert_sequence)},
		{"pps_info.clear_sequence", unsafe.Offsetof(gi.ClearSequence), unsafe.Offsetof(ci.clear_sequence)},
		{"pps_info.assert_tu", unsafe.Offsetof(gi.AssertTime), unsafe.Offsetof(ci.assert_tu)},
		{"pps_info.clear_tu", unsafe.Offsetof(gi.ClearTime), unsafe.Offsetof(ci.clear_tu)},
		{"pps_info.current_mode", unsafe.Offsetof(gi.CurrentMode), unsafe.Offsetof(ci.current_mode)},

		{"sizeof pps_params", unsafe.Sizeof(gp), unsafe.Sizeof(cp)},
		{"pps_params.api_version", unsafe.Offsetof(gp.APIVersion), unsafe.Offsetof(cp.api_version)},
		{"pps_params.mode", unsafe.Offsetof(gp.Mode), unsafe.Offsetof(cp.mode)},
		{"pps_params.assert_off_tu", unsafe.Offsetof(gp.AssertOffset), unsafe.Offsetof(cp.assert_off_tu)},
		{"pps_params.clear_off_tu", unsafe.Offsetof(gp.ClearOffset), unsafe.Offsetof(cp.clear_off_tu)},

		{"sizeof pps_fetch_args", unsafe.Sizeof(gf), unsafe.Sizeof(cf)},
		{"pps_fetch_args.tsformat", unsafe.Offsetof(gf.TSFormat), unsafe.Offsetof(cf.tsformat)},
		{"pps_fetch_args.pps_info_buf", unsafe.Offsetof(gf.Info), unsafe.Offsetof(cf.pps_info_buf)},
		{"pps_fetch_args.timeout", unsafe.Offsetof(gf.Timeout), unsafe.Offsetof(cf.timeout)},

		{"PPS_API_VERS_1", ppsAPIVersion, uintptr(C.PPS_API_VERS_1)},
		{"PPS_CAPTUREASSERT", ppsCaptureAssert, uintptr(C.PPS_CAPTUREASSERT)},
		{"PPS_CAPTURECLEAR", ppsCaptureClear, uintptr(C.PPS_CAPTURECLEAR)},
		{"PPS_TSFMT_TSPEC", ppsTSFmtTSpec, uintptr(C.PPS_TSFMT_TSPEC)},

		{"PPS_IOC_CREATE", ppsIOCCreate, uintptr(C.PPS_IOC_CREATE)},
		{"PPS_IOC_DESTROY", ppsIOCDestroy, uintptr(C.PPS_IOC_DESTROY)},
		{"PPS_IOC_SETPARAMS", ppsIOCSetParams, uintptr(C.PPS_IOC_SETPARAMS)},
		{"PPS_IOC_GETCAP", ppsIOCGetCap, uintptr(C.PPS_IOC_GETCAP)},
		{"PPS_IOC_FETCH", ppsIOCFetch, uintptr(C.PPS_IOC_FETCH)},
	}
}
